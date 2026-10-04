package backup

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/codemeapixel/nobackups/internal/storage"
)

const timeLayout = "20060102T150405Z"

var snapRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)-(\d{8}T\d{6}Z)\.tar(\.zst|\.gz)?(\.age)?$`)

type Snapshot struct {
	Key         string
	Job         string
	Time        time.Time
	Size        int64
	Compression string
	Encrypted   bool
}

func (s Snapshot) ID() string { return s.Time.UTC().Format(timeLayout) }

func snapshotKey(job string, t time.Time, compression string, encrypted bool) string {
	key := fmt.Sprintf("%s/%s-%s.tar", job, job, t.UTC().Format(timeLayout))
	switch compression {
	case "zstd":
		key += ".zst"
	case "gzip":
		key += ".gz"
	}
	if encrypted {
		key += ".age"
	}
	return key
}

func parseSnapshot(o storage.Object) (Snapshot, bool) {
	m := snapRe.FindStringSubmatch(o.Key)
	if m == nil || m[1] != m[2] {
		return Snapshot{}, false
	}
	t, err := time.Parse(timeLayout, m[3])
	if err != nil {
		return Snapshot{}, false
	}
	s := Snapshot{Key: o.Key, Job: m[1], Time: t, Size: o.Size, Compression: "none", Encrypted: m[5] != ""}
	switch m[4] {
	case ".zst":
		s.Compression = "zstd"
	case ".gz":
		s.Compression = "gzip"
	}
	return s, true
}

func ListSnapshots(ctx context.Context, b storage.Backend, job string) ([]Snapshot, error) {
	objs, err := b.List(ctx, job+"/")
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, o := range objs {
		if s, ok := parseSnapshot(o); ok && s.Job == job {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

func FindSnapshot(snaps []Snapshot, id string) (Snapshot, error) {
	if len(snaps) == 0 {
		return Snapshot{}, fmt.Errorf("no snapshots found")
	}
	if id == "" || id == "latest" {
		return snaps[0], nil
	}
	for _, s := range snaps {
		if s.ID() == id || s.Key == id {
			return s, nil
		}
	}
	return Snapshot{}, fmt.Errorf("snapshot %q not found", id)
}

func SelectExpired(snaps []Snapshot, keepLast, keepDays int, now time.Time) []Snapshot {
	if keepLast <= 0 && keepDays <= 0 {
		return nil
	}
	cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour)
	var out []Snapshot
	for i, s := range snaps {
		if i == 0 || i < keepLast || (keepDays > 0 && s.Time.After(cutoff)) {
			continue
		}
		out = append(out, s)
	}
	return out
}
