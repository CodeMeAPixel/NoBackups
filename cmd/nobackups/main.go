// Command nobackups backs up directories on Linux servers to S3-compatible
// object storage or local disks.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/codemeapixel/nobackups/internal/backup"
	"github.com/codemeapixel/nobackups/internal/config"
	"github.com/codemeapixel/nobackups/internal/notify"
	"github.com/codemeapixel/nobackups/internal/storage"
)

var version = "dev"

//go:embed example.yaml
var exampleConfig []byte

const usage = `nobackups - simple backups for Linux servers

Usage:
  nobackups [-c config] <command> [flags]

Commands:
  install                   Install this binary, config and systemd unit (root)
  uninstall                 Remove the binary and unit, keeping config and backups
  init                      Write an example config file
  validate                  Check the config file
  check                     Test that every destination is reachable and writable
  list                      Show configured jobs and destinations
  run [job...] | --all      Run backup jobs now
  daemon                    Run jobs on their schedules (for systemd)
  snapshots <job>           List stored snapshots of a job
  restore <job> --target D  Restore a snapshot into directory D
  prune [job...]            Apply retention policies now
  status                    Show the result of each job's last run
  test-notify [job]         Send a sample notification to the configured webhooks
  version                   Print the version

The config path defaults to $NOBACKUPS_CONFIG or ` + config.DefaultPath + `.
Run 'nobackups <command> -h' for command flags.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultConfigPath() string {
	if p := os.Getenv("NOBACKUPS_CONFIG"); p != "" {
		return p
	}
	return config.DefaultPath
}

func run(args []string) error {
	global := flag.NewFlagSet("nobackups", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	cfgPath := global.String("c", defaultConfigPath(), "config file")
	global.StringVar(cfgPath, "config", defaultConfigPath(), "config file")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if global.NArg() == 0 {
		global.Usage()
		return nil
	}
	cmd, rest := global.Arg(0), global.Args()[1:]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.StringVar(cfgPath, "c", *cfgPath, "config file")
	fs.StringVar(cfgPath, "config", *cfgPath, "config file")
	verbose := fs.Bool("v", false, "verbose (debug) logging")

	switch cmd {
	case "version", "--version":
		fmt.Println("nobackups", version)
		return nil
	case "help", "-h", "--help":
		global.Usage()
		return nil
	case "init":
		force := fs.Bool("force", false, "overwrite an existing file")
		if err := parse(fs, rest); err != nil {
			return err
		}
		return cmdInit(*cfgPath, *force)
	case "install", "uninstall":
		var o installOptions
		fs.StringVar(&o.Root, "root", "", "install under this directory instead of / (for packaging; skips systemctl)")
		fs.StringVar(&o.BinDir, "bin-dir", "/usr/local/bin", "where to put the binary")
		fs.StringVar(&o.ConfigDir, "config-dir", "/etc/nobackups", "where config.yaml and nobackups.env live")
		fs.StringVar(&o.UnitDir, "unit-dir", "/etc/systemd/system", "where to put the systemd unit")
		fs.BoolVar(&o.NoSystemd, "no-systemd", false, "don't run systemctl")
		if err := parse(fs, rest); err != nil {
			return err
		}
		if cmd == "install" {
			return cmdInstall(o)
		}
		return cmdUninstall(o)
	}

	var (
		all, jsonOut           bool
		dest, snap, target, id string
		event                  string
		paths                  multiFlag
	)
	switch cmd {
	case "run":
		fs.BoolVar(&all, "all", false, "run every job")
	case "snapshots":
		fs.StringVar(&dest, "dest", "", "destination (default: all of the job's destinations)")
	case "restore":
		fs.StringVar(&dest, "dest", "", "destination to restore from (default: the job's first)")
		fs.StringVar(&snap, "snapshot", "latest", "snapshot ID (timestamp) or 'latest'")
		fs.StringVar(&target, "target", "", "directory to restore into (required)")
		fs.StringVar(&id, "identity", "", "age identity file (for recipient-encrypted jobs)")
		fs.Var(&paths, "path", "only restore this path, e.g. etc/nginx (repeatable)")
	case "status":
		fs.BoolVar(&jsonOut, "json", false, "output JSON")
	case "test-notify":
		fs.StringVar(&event, "event", "failure", "event to simulate: success, warning or failure")
	}
	if err := parse(fs, rest); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel, *verbose)
	runner := backup.NewRunner(cfg, log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "validate":
		fmt.Printf("%s: OK (%d jobs, %d destinations)\n", cfg.Path, len(cfg.Jobs), len(cfg.Destinations))
		return nil
	case "check":
		return cmdCheck(ctx, runner)
	case "list":
		return cmdList(cfg)
	case "run":
		return cmdRun(ctx, runner, fs.Args(), all)
	case "daemon":
		return runDaemon(ctx, *cfgPath, *verbose, cfg, log)
	case "snapshots":
		return cmdSnapshots(ctx, runner, fs.Args(), dest)
	case "restore":
		return cmdRestore(ctx, runner, fs.Args(), backup.RestoreOptions{
			Destination: dest, Snapshot: snap, Target: target, Paths: paths, IdentityFile: id,
		})
	case "prune":
		jobs, err := selectJobs(cfg, fs.Args(), len(fs.Args()) == 0)
		if err != nil {
			return err
		}
		var errs []error
		for _, j := range jobs {
			errs = append(errs, runner.Prune(ctx, j))
		}
		return errors.Join(errs...)
	case "status":
		return cmdStatus(cfg, jsonOut)
	case "test-notify":
		return cmdTestNotify(ctx, cfg, log, fs.Args(), event)
	}
	return fmt.Errorf("unknown command %q (see 'nobackups help')", cmd)
}

// parse allows flags after positional arguments, e.g. "restore etc --target /tmp/x".
func parse(fs *flag.FlagSet, args []string) error {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return fs.Parse(pos) // re-parse to set fs.Args() to the positionals
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func newLogger(level string, verbose bool) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	if verbose {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func selectJobs(cfg *config.Config, names []string, all bool) ([]*config.Job, error) {
	if all {
		return cfg.Jobs, nil
	}
	if len(names) == 0 {
		return nil, errors.New("specify one or more job names, or --all")
	}
	var out []*config.Job
	for _, n := range names {
		j, ok := cfg.Job(n)
		if !ok {
			return nil, fmt.Errorf("unknown job %q", n)
		}
		out = append(out, j)
	}
	return out, nil
}

func cmdInit(path string, force bool) error {
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, exampleConfig, 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote example config to %s\nedit it, then run: nobackups validate && nobackups check\n", path)
	return nil
}

func cmdCheck(ctx context.Context, r *backup.Runner) error {
	failed := 0
	for _, name := range r.Cfg.DestinationNames() {
		err := checkDestination(ctx, r, name)
		if err != nil {
			failed++
			fmt.Printf("✗ %-20s %v\n", name, err)
		} else {
			fmt.Printf("✓ %-20s ok\n", name)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d destination(s) failed", failed)
	}
	return nil
}

func checkDestination(ctx context.Context, r *backup.Runner, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	b, err := r.Backend(name)
	if err != nil {
		return err
	}
	if s3, ok := b.(*storage.S3); ok {
		if err := s3.CheckBucket(ctx); err != nil {
			return err
		}
	}
	key := fmt.Sprintf(".nobackups-check-%s-%d", r.Cfg.Hostname, time.Now().UnixNano())
	if err := b.Put(ctx, key, strings.NewReader("nobackups write test\n")); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if s3, ok := b.(*storage.S3); ok {
		if err := s3.CheckMultipart(ctx, key+"-multipart"); err != nil {
			return fmt.Errorf("multipart write: %w", err)
		}
	}
	if _, err := b.List(ctx, ""); err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

func cmdList(cfg *config.Config) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DESTINATION\tTYPE\tLOCATION")
	for _, n := range cfg.DestinationNames() {
		d := cfg.Destinations[n]
		loc := d.Path
		if d.Type == "s3" {
			loc = fmt.Sprintf("s3://%s/%s (%s)", d.Bucket, d.Prefix, d.Endpoint)
		} else if d.Prefix != "" {
			loc = filepath.Join(d.Path, d.Prefix)
		}
		kind := d.Type
		if d.Provider != "" {
			kind = d.Provider
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", n, kind, loc)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "JOB\tSCHEDULE\tNEXT RUN\tDESTINATIONS\tSOURCES")
	for _, j := range cfg.Jobs {
		sched, next := "manual", "-"
		if j.Schedule != "" {
			sched = j.Schedule
			if s, err := cronParser.Parse(j.Schedule); err == nil {
				next = s.Next(time.Now()).Format("2006-01-02 15:04")
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", j.Name, sched, next,
			strings.Join(j.Destinations, ","), strings.Join(j.Sources, ","))
	}
	return w.Flush()
}

func cmdRun(ctx context.Context, r *backup.Runner, names []string, all bool) error {
	jobs, err := selectJobs(r.Cfg, names, all)
	if err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		res, err := r.Run(ctx, j)
		if res != nil {
			notify.Send(context.WithoutCancel(ctx), r.Cfg.Notify, j, res, r.Log)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("job %s: %w", j.Name, err))
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

func cmdSnapshots(ctx context.Context, r *backup.Runner, args []string, dest string) error {
	if len(args) != 1 {
		return errors.New("usage: nobackups snapshots <job> [--dest name]")
	}
	job, ok := r.Cfg.Job(args[0])
	if !ok {
		return fmt.Errorf("unknown job %q", args[0])
	}
	dests := job.Destinations
	if dest != "" {
		dests = []string{dest}
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DESTINATION\tSNAPSHOT\tTIME\tSIZE\tENCRYPTED")
	var errs []error
	for _, d := range dests {
		b, err := r.Backend(d)
		if err != nil {
			return err
		}
		snaps, err := backup.ListSnapshots(ctx, b, job.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d, err))
			continue
		}
		for _, s := range snaps {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\n", d, s.ID(), s.Time.Local().Format("2006-01-02 15:04:05"),
				notify.HumanBytes(s.Size), s.Encrypted)
		}
	}
	w.Flush()
	return errors.Join(errs...)
}

func cmdRestore(ctx context.Context, r *backup.Runner, args []string, opts backup.RestoreOptions) error {
	if len(args) != 1 || opts.Target == "" {
		return errors.New("usage: nobackups restore <job> --target <dir> [--snapshot ID] [--dest name] [--path p]")
	}
	job, ok := r.Cfg.Job(args[0])
	if !ok {
		return fmt.Errorf("unknown job %q", args[0])
	}
	if t, err := filepath.Abs(opts.Target); err == nil && t == "/" {
		r.Log.Warn("restoring directly into / will overwrite live files")
	}
	snap, st, err := r.Restore(ctx, job, opts)
	if err != nil {
		return err
	}
	fmt.Printf("restored snapshot %s of %s into %s: %d files, %d dirs, %s\n",
		snap.ID(), job.Name, opts.Target, st.Files, st.Dirs, notify.HumanBytes(st.Bytes))
	if st.Warnings > 0 {
		fmt.Printf("%d warnings (see log output)\n", st.Warnings)
	}
	return nil
}

func cmdStatus(cfg *config.Config, jsonOut bool) error {
	results, err := backup.ReadStatuses(cfg.StateDir)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(os.Stdout, results)
	}
	byJob := map[string]backup.Result{}
	for _, r := range results {
		byJob[r.Job] = r
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tLAST RUN\tSTATUS\tDURATION\tSTORED\tERROR")
	for _, j := range cfg.Jobs {
		r, ok := byJob[j.Name]
		if !ok {
			fmt.Fprintf(w, "%s\tnever\t-\t-\t-\t\n", j.Name)
			continue
		}
		status := "ok"
		if !r.Success {
			status = "FAILED"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", j.Name, r.Started.Local().Format("2006-01-02 15:04"),
			status, r.Duration, notify.HumanBytes(r.StoredBytes), r.Error)
	}
	return w.Flush()
}

func cmdTestNotify(ctx context.Context, cfg *config.Config, log *slog.Logger, args []string, event string) error {
	var job *config.Job
	name := "test-job"
	if len(args) > 0 {
		j, ok := cfg.Job(args[0])
		if !ok {
			return fmt.Errorf("unknown job %q", args[0])
		}
		job, name = j, j.Name
	}
	now := time.Now()
	res := &backup.Result{
		Job: name, Host: cfg.Hostname, Success: true, Started: now.Add(-42 * time.Second), Finished: now,
		Duration: "42s", Files: 1234, Bytes: 5 << 30, StoredBytes: 2 << 30,
		Destinations: []backup.DestResult{{Destination: "example", Key: "example-key"}},
	}
	switch event {
	case "success":
	case "warning":
		res.Warnings = 3
	case "failure":
		res.Success = false
		res.Error = "this is a test notification from 'nobackups test-notify'"
		res.Destinations[0].Error = "simulated upload error"
	default:
		return fmt.Errorf("unknown event %q (expected success, warning or failure)", event)
	}
	// Send to every target regardless of its "on" filter.
	all := func(n config.Notify) config.Notify {
		var out config.Notify
		for _, w := range n.Webhooks {
			w.On = config.NotifyEvents
			out.Webhooks = append(out.Webhooks, w)
		}
		for _, d := range n.Discord {
			d.On = config.NotifyEvents
			out.Discord = append(out.Discord, d)
		}
		return out
	}
	global := all(cfg.Notify)
	count := len(global.Webhooks) + len(global.Discord)
	if job != nil {
		jc := *job
		jc.Notify = all(job.Notify)
		job = &jc
		count += len(job.Notify.Webhooks) + len(job.Notify.Discord)
	}
	if count == 0 {
		return errors.New("no notification targets configured")
	}
	notify.Send(ctx, global, job, res, log)
	fmt.Printf("sent %s test notification to %d target(s); check the log above for errors\n", event, count)
	return nil
}
