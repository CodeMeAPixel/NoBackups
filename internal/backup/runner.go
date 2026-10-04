// Package backup runs backup jobs, applies retention and restores snapshots.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/codemeapixel/nobackups/internal/archive"
	"github.com/codemeapixel/nobackups/internal/config"
	"github.com/codemeapixel/nobackups/internal/storage"
)

var ErrLocked = errors.New("job is already running")

type DestResult struct {
	Destination string   `json:"destination"`
	Key         string   `json:"key,omitempty"`
	Error       string   `json:"error,omitempty"`
	Pruned      []string `json:"pruned,omitempty"`
}

type Result struct {
	Job          string       `json:"job"`
	Host         string       `json:"host"`
	Success      bool         `json:"success"`
	Error        string       `json:"error,omitempty"`
	Started      time.Time    `json:"started"`
	Finished     time.Time    `json:"finished"`
	Duration     string       `json:"duration"`
	Files        int64        `json:"files"`
	Bytes        int64        `json:"bytes"`
	StoredBytes  int64        `json:"stored_bytes"`
	Warnings     int64        `json:"warnings"`
	Destinations []DestResult `json:"destinations"`
}

type Runner struct {
	Cfg *config.Config
	Log *slog.Logger

	mu       sync.Mutex
	backends map[string]storage.Backend
}

func NewRunner(cfg *config.Config, log *slog.Logger) *Runner {
	return &Runner{Cfg: cfg, Log: log, backends: map[string]storage.Backend{}}
}

func (r *Runner) Backend(name string) (storage.Backend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.backends[name]; ok {
		return b, nil
	}
	d, ok := r.Cfg.Destinations[name]
	if !ok {
		return nil, fmt.Errorf("unknown destination %q", name)
	}
	b, err := storage.New(d)
	if err != nil {
		return nil, err
	}
	r.backends[name] = b
	return b, nil
}

func (r *Runner) Run(ctx context.Context, job *config.Job) (*Result, error) {
	log := r.Log.With("job", job.Name)
	res := &Result{Job: job.Name, Host: r.Cfg.Hostname, Started: time.Now()}

	unlock, err := lockJob(r.Cfg.StateDir, job.Name)
	if errors.Is(err, ErrLocked) {
		return nil, err
	}
	if err != nil {
		res.Finished = time.Now()
		res.Duration = "0s"
		res.Error = err.Error()
		log.Error("backup failed", "err", err)
		return res, err
	}
	defer unlock()

	if job.Timeout.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, job.Timeout.Duration)
		defer cancel()
	}

	log.Info("backup started")
	runErr := r.runHooks(ctx, log, job, job.Hooks.Before, "")
	if runErr != nil {
		runErr = fmt.Errorf("before hook: %w", runErr)
	} else {
		runErr = r.backup(ctx, log, job, res)
	}

	status := "success"
	if runErr != nil {
		status = "failure"
	}
	if err := r.runHooks(context.WithoutCancel(ctx), log, job, job.Hooks.After, status); err != nil {
		log.Error("after hook failed", "err", err)
		if runErr == nil {
			runErr = fmt.Errorf("after hook: %w", err)
		}
	}

	res.Finished = time.Now()
	res.Duration = res.Finished.Sub(res.Started).Round(time.Millisecond).String()
	res.Success = runErr == nil
	if runErr != nil {
		res.Error = runErr.Error()
		log.Error("backup failed", "err", runErr, "duration", res.Duration)
	} else {
		log.Info("backup finished", "files", res.Files, "bytes", res.Bytes, "stored_bytes", res.StoredBytes,
			"warnings", res.Warnings, "duration", res.Duration)
	}
	if err := writeStatus(r.Cfg.StateDir, res); err != nil {
		log.Warn("cannot write status file", "err", err)
	}
	return res, runErr
}

func (r *Runner) backup(ctx context.Context, log *slog.Logger, job *config.Job, res *Result) error {
	var recips []age.Recipient
	if job.Encryption != nil {
		var err error
		if recips, err = recipients(job.Encryption); err != nil {
			return fmt.Errorf("encryption: %w", err)
		}
	}

	backends := make([]storage.Backend, 0, len(job.Destinations))
	for _, name := range job.Destinations {
		b, err := r.Backend(name)
		if err != nil {
			return err
		}
		backends = append(backends, b)
	}

	key := snapshotKey(job.Name, res.Started, job.Compression, recips != nil)
	uploadCtx, cancelUploads := context.WithCancel(ctx)
	defer cancelUploads()

	pipes := make([]*io.PipeWriter, len(backends))
	results := make([]DestResult, len(backends))
	var wg sync.WaitGroup
	for i, b := range backends {
		pr, pw := io.Pipe()
		pipes[i] = pw
		results[i] = DestResult{Destination: b.Name(), Key: key}
		wg.Add(1)
		go func(i int, b storage.Backend) {
			defer wg.Done()
			err := b.Put(uploadCtx, key, pr)
			if err == nil {
				_, err = io.Copy(io.Discard, pr)
			}
			if err != nil {
				pr.CloseWithError(err)
				results[i].Error = err.Error()
				log.Error("upload failed", "destination", b.Name(), "err", err)
				return
			}
			pr.Close()
		}(i, b)
	}

	fan := newFanout(pipes)
	counter := &countingWriter{w: fan}
	stats, archErr := writeArchive(counter, job, recips, log)
	fan.closeAll(archErr)
	if archErr != nil {
		cancelUploads()
	}
	wg.Wait()

	res.Files, res.Bytes, res.Warnings = stats.Files, stats.Bytes, stats.Warnings
	res.StoredBytes = counter.n

	if archErr != nil {
		for i := range results {
			if results[i].Error == "" {
				results[i].Error = archErr.Error()
			}
			results[i].Key = ""
		}
		res.Destinations = results
		return fmt.Errorf("archive: %w", archErr)
	}

	var failed []string
	for i := range results {
		if results[i].Error != "" {
			results[i].Key = ""
			failed = append(failed, results[i].Destination)
			continue
		}
		log.Info("uploaded", "destination", results[i].Destination, "key", key)
		pruned, err := r.prune(ctx, log, backends[i], job)
		results[i].Pruned = pruned
		if err != nil {
			log.Warn("retention failed", "destination", results[i].Destination, "err", err)
		}
	}
	res.Destinations = results
	if len(failed) > 0 {
		return fmt.Errorf("upload failed for: %s", strings.Join(failed, ", "))
	}
	return nil
}

func writeArchive(w io.Writer, job *config.Job, recips []age.Recipient, log *slog.Logger) (archive.Stats, error) {
	var out io.WriteCloser = nopWriteCloser{w}
	if recips != nil {
		enc, err := age.Encrypt(w, recips...)
		if err != nil {
			return archive.Stats{}, err
		}
		out = enc
	}
	comp, err := compressWriter(out, job.Compression, job.CompressionLevel)
	if err != nil {
		return archive.Stats{}, err
	}
	stats, err := archive.Create(comp, archive.Options{
		Sources:       job.Sources,
		Exclude:       job.Exclude,
		OneFileSystem: job.OneFileSystem,
		Log:           log,
	})
	if err != nil {
		return stats, err
	}
	if err := comp.Close(); err != nil {
		return stats, err
	}
	return stats, out.Close()
}

func (r *Runner) Prune(ctx context.Context, job *config.Job) error {
	var errs []error
	for _, name := range job.Destinations {
		b, err := r.Backend(name)
		if err == nil {
			_, err = r.prune(ctx, r.Log.With("job", job.Name), b, job)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) prune(ctx context.Context, log *slog.Logger, b storage.Backend, job *config.Job) ([]string, error) {
	if job.Retention.KeepLast == 0 && job.Retention.KeepDays == 0 {
		return nil, nil
	}
	snaps, err := ListSnapshots(ctx, b, job.Name)
	if err != nil {
		return nil, err
	}
	var pruned []string
	var errs []error
	for _, s := range SelectExpired(snaps, job.Retention.KeepLast, job.Retention.KeepDays, time.Now()) {
		if err := b.Delete(ctx, s.Key); err != nil {
			errs = append(errs, err)
			continue
		}
		log.Info("pruned snapshot", "destination", b.Name(), "snapshot", s.ID())
		pruned = append(pruned, s.Key)
	}
	return pruned, errors.Join(errs...)
}

func (r *Runner) runHooks(ctx context.Context, log *slog.Logger, job *config.Job, cmds []string, status string) error {
	for _, c := range cmds {
		log.Info("running hook", "cmd", c)
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", c)
		cmd.Env = append(os.Environ(), "NOBACKUPS_JOB="+job.Name, "NOBACKUPS_HOST="+r.Cfg.Hostname)
		if status != "" {
			cmd.Env = append(cmd.Env, "NOBACKUPS_STATUS="+status)
		}
		out, err := cmd.CombinedOutput()
		if len(out) > 0 {
			log.Info("hook output", "cmd", c, "output", strings.TrimSpace(string(out)))
		}
		if err != nil {
			return fmt.Errorf("%q: %w", c, err)
		}
	}
	return nil
}

type RestoreOptions struct {
	Destination  string
	Snapshot     string
	Target       string
	Paths        []string
	IdentityFile string
}

func (r *Runner) Restore(ctx context.Context, job *config.Job, opts RestoreOptions) (Snapshot, archive.Stats, error) {
	dest := opts.Destination
	if dest == "" {
		dest = job.Destinations[0]
	}
	b, err := r.Backend(dest)
	if err != nil {
		return Snapshot{}, archive.Stats{}, err
	}
	snaps, err := ListSnapshots(ctx, b, job.Name)
	if err != nil {
		return Snapshot{}, archive.Stats{}, err
	}
	snap, err := FindSnapshot(snaps, opts.Snapshot)
	if err != nil {
		return Snapshot{}, archive.Stats{}, err
	}

	body, err := b.Get(ctx, snap.Key)
	if err != nil {
		return snap, archive.Stats{}, err
	}
	defer body.Close()

	var src io.Reader = body
	if snap.Encrypted {
		enc := job.Encryption
		if opts.IdentityFile != "" {
			enc = &config.Encryption{IdentityFile: opts.IdentityFile}
		}
		ids, err := identities(enc)
		if err != nil {
			return snap, archive.Stats{}, err
		}
		if src, err = age.Decrypt(body, ids...); err != nil {
			return snap, archive.Stats{}, fmt.Errorf("decrypt: %w", err)
		}
	}
	dec, err := decompressReader(src, snap.Compression)
	if err != nil {
		return snap, archive.Stats{}, err
	}
	defer dec.Close()

	st, err := archive.Extract(dec, archive.ExtractOptions{Target: opts.Target, Paths: opts.Paths, Log: r.Log})
	return snap, st, err
}
