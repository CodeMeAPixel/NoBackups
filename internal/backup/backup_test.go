package backup

import (
	"context"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/codemeapixel/nobackups/internal/config"
)

func TestSelectExpired(t *testing.T) {
	now := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	var snaps []Snapshot
	for i := range 10 { // one per day, newest first
		snaps = append(snaps, Snapshot{Key: string(rune('a' + i)), Time: now.Add(-time.Duration(i) * 24 * time.Hour)})
	}
	count := func(keepLast, keepDays int) int { return len(SelectExpired(snaps, keepLast, keepDays, now)) }
	if n := count(0, 0); n != 0 {
		t.Errorf("no policy: expired %d", n)
	}
	if n := count(3, 0); n != 7 {
		t.Errorf("keep_last 3: expired %d, want 7", n)
	}
	if n := count(0, 5); n != 5 { // days 0-4 kept
		t.Errorf("keep_days 5: expired %d, want 5", n)
	}
	if n := count(7, 2); n != 3 { // union: newest 7 kept
		t.Errorf("keep_last 7 + keep_days 2: expired %d, want 3", n)
	}
	if n := len(SelectExpired(snaps[9:], 0, 1, now)); n != 0 {
		t.Errorf("the newest snapshot must never expire")
	}
}

func TestSnapshotKeyRoundTrip(t *testing.T) {
	ts := time.Date(2026, 10, 4, 19, 5, 0, 0, time.UTC)
	key := snapshotKey("my-job", ts, "zstd", true)
	if key != "my-job/my-job-20261004T190500Z.tar.zst.age" {
		t.Fatalf("key = %s", key)
	}
	s, ok := parseSnapshot(storageObject(key))
	if !ok || s.Job != "my-job" || !s.Time.Equal(ts) || s.Compression != "zstd" || !s.Encrypted {
		t.Fatalf("parse = %+v %v", s, ok)
	}
	if _, ok := parseSnapshot(storageObject("my-job/other-20261004T190500Z.tar")); ok {
		t.Error("should not match a different job's file")
	}
}

// TestEndToEnd backs up to a fake S3 server and a local directory, prunes,
// and restores, for every compression mode with and without encryption.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end test")
	}
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	srv := httptest.NewTLSServer(faker.Server())
	defer srv.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	if err := backend.CreateBucket("backups"); err != nil {
		t.Fatal(err)
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(t.TempDir(), "key.txt")
	os.WriteFile(idFile, []byte(id.String()+"\n"), 0o600)

	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello world"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "big.bin"), []byte(strings.Repeat("0123456789", 1_200_000)), 0o600)

	cases := []struct {
		name        string
		compression string
		enc         *config.Encryption
	}{
		{"zstd-plain", "zstd", nil},
		{"gzip-pass", "gzip", &config.Encryption{Passphrase: "correct horse"}},
		{"none-recipient", "none", &config.Encryption{Recipients: []string{id.Recipient().String()}, IdentityFile: idFile}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
state_dir: ` + t.TempDir() + `
hostname: testhost
destinations:
  s3:
    type: s3
    endpoint: ` + srv.URL + `
    bucket: backups
    access_key_id: x
    secret_access_key: y
    path_style: true
    ca_file: ` + caFile + `
    part_size_mb: 5
    prefix: "{hostname}"
  disk:
    type: local
    path: ` + t.TempDir() + `
jobs:
  - name: ` + tc.name + `
    sources: [` + src + `]
    destinations: [s3, disk]
    compression: ` + tc.compression + `
    retention: {keep_last: 2}
`
			cfg, err := config.Parse([]byte(yaml), "test.yaml")
			if err != nil {
				t.Fatal(err)
			}
			job := cfg.Jobs[0]
			job.Encryption = tc.enc
			r := NewRunner(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			ctx := context.Background()

			// Three runs with keep_last 2 → one pruned per destination.
			for i := range 3 {
				res, err := r.Run(ctx, job)
				if err != nil {
					t.Fatalf("run %d: %v", i, err)
				}
				if res.Files != 2 || !res.Success {
					t.Fatalf("run %d result: %+v", i, res)
				}
				time.Sleep(1100 * time.Millisecond) // keys have 1s resolution
			}
			for _, d := range job.Destinations {
				b, _ := r.Backend(d)
				snaps, err := ListSnapshots(ctx, b, job.Name)
				if err != nil {
					t.Fatal(err)
				}
				if len(snaps) != 2 {
					t.Errorf("%s: %d snapshots after retention, want 2", d, len(snaps))
				}
			}

			for _, d := range job.Destinations {
				target := t.TempDir()
				_, st, err := r.Restore(ctx, job, RestoreOptions{Destination: d, Target: target})
				if err != nil {
					t.Fatalf("restore from %s: %v", d, err)
				}
				if st.Files != 2 {
					t.Errorf("restored %d files", st.Files)
				}
				got, err := os.ReadFile(filepath.Join(target, src, "hello.txt"))
				if err != nil || string(got) != "hello world" {
					t.Errorf("restored content %q, %v", got, err)
				}
				big, _ := os.ReadFile(filepath.Join(target, src, "sub", "big.bin"))
				if len(big) != 12_000_000 {
					t.Errorf("big.bin is %d bytes", len(big))
				}
			}
		})
	}
}

func TestOneDestinationFailing(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "f"), []byte("data"), 0o644)
	good := t.TempDir()
	yaml := `
state_dir: ` + t.TempDir() + `
destinations:
  good: {type: local, path: ` + good + `}
  bad:  {type: s3, endpoint: "http://127.0.0.1:1", bucket: b, access_key_id: x, secret_access_key: y}
jobs:
  - name: j
    sources: [` + src + `]
    destinations: [bad, good]
`
	cfg, err := config.Parse([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := NewRunner(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := r.Run(context.Background(), cfg.Jobs[0])
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("expected failure naming 'bad', got %v", err)
	}
	b, _ := r.Backend("good")
	snaps, _ := ListSnapshots(context.Background(), b, "j")
	if len(snaps) != 1 {
		t.Errorf("good destination should still have the snapshot, has %d", len(snaps))
	}
	if res.Destinations[1].Error != "" || res.Destinations[0].Error == "" {
		t.Errorf("per-destination results wrong: %+v", res.Destinations)
	}
}

func TestLockPreventsConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockJob(dir, "j")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockJob(dir, "j"); err == nil {
		t.Fatal("second lock should fail")
	}
	unlock()
	if u, err := lockJob(dir, "j"); err != nil {
		t.Fatal(err)
	} else {
		u()
	}
}

func TestDatabaseDumpsAndFailedDumpSkipsRetention(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "pg_dump"), []byte(`#!/bin/sh
for a in "$@"; do db=$a; done
[ "$db" = broken ] && { echo "pg_dump: error: database \"broken\" does not exist" >&2; exit 1; }
printf 'dump of %s as %s\n' "$db" "$PGPASSWORD"
`), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "f"), []byte("file"), 0o644)
	store := t.TempDir()
	yaml := `
state_dir: ` + t.TempDir() + `
destinations:
  disk: {type: local, path: ` + store + `}
jobs:
  - name: j
    sources: [` + src + `]
    databases:
      - {type: postgres, database: app, password: pw}
      - {type: postgres, database: broken}
    destinations: [disk]
    retention: {keep_last: 1}
`
	cfg, err := config.Parse([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	job := cfg.Jobs[0]
	r := NewRunner(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	job.Databases = job.Databases[:1]
	if _, err := r.Run(ctx, job); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)

	cfg2, _ := config.Parse([]byte(yaml), "test.yaml")
	job2 := cfg2.Jobs[0]
	res, err := r.Run(ctx, job2)
	if err == nil || !strings.Contains(err.Error(), `database "broken" does not exist`) {
		t.Fatalf("expected the failed dump to fail the job, got %v", err)
	}
	if len(res.Databases) != 2 || res.Databases[0].Error != "" || res.Databases[1].Error == "" {
		t.Fatalf("database results: %+v", res.Databases)
	}

	b, _ := r.Backend("disk")
	snaps, _ := ListSnapshots(ctx, b, "j")
	if len(snaps) != 2 {
		t.Fatalf("retention must be skipped after a failed dump; have %d snapshots", len(snaps))
	}

	target := t.TempDir()
	if _, _, err := r.Restore(ctx, job2, RestoreOptions{Target: target}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "nobackups-databases", "postgres-app.dump"))
	if err != nil || string(got) != "dump of app as pw\n" {
		t.Fatalf("restored dump %q, %v", got, err)
	}
	if _, err := os.ReadFile(filepath.Join(target, src, "f")); err != nil {
		t.Fatal("files should be backed up alongside the dumps")
	}
	if entries, _ := os.ReadDir(filepath.Join(cfg.StateDir, "dumps")); len(entries) != 0 {
		t.Fatalf("dump files left behind: %v", entries)
	}
}
