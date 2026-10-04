package archive

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExcluded(t *testing.T) {
	pats := []string{"*.tmp", "node_modules", "/home/*/Downloads"}
	cases := map[string]bool{
		"/srv/a.tmp":                  true,
		"/srv/app/node_modules":       true,
		"/srv/app/node_modules/x.js":  false,
		"/home/bob/Downloads":         true,
		"/home/bob/Downloads/big.iso": true,
		"/home/bob/Documents/a.txt":   false,
		"/srv/tmp":                    false,
	}
	for p, want := range cases {
		if got := Excluded(p, pats); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	src := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(src, "app", "cache"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "app", "config.ini"), []byte("a=1\n"), 0o640))
	must(t, os.WriteFile(filepath.Join(src, "app", "cache", "junk"), []byte("x"), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "app", "x.tmp"), []byte("x"), 0o644))
	must(t, os.Symlink("config.ini", filepath.Join(src, "app", "link")))

	var buf bytes.Buffer
	st, err := Create(&buf, Options{Sources: []string{filepath.Join(src, "app")}, Exclude: []string{"cache", "*.tmp"}})
	must(t, err)
	if st.Files != 1 {
		t.Errorf("files = %d, want 1 (config.ini)", st.Files)
	}

	dst := t.TempDir()
	_, err = Extract(&buf, ExtractOptions{Target: dst})
	must(t, err)
	restored := filepath.Join(dst, src, "app")
	b, err := os.ReadFile(filepath.Join(restored, "config.ini"))
	must(t, err)
	if string(b) != "a=1\n" {
		t.Errorf("content = %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(restored, "config.ini")); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if l, err := os.Readlink(filepath.Join(restored, "link")); err != nil || l != "config.ini" {
		t.Errorf("symlink = %q, %v", l, err)
	}
	for _, p := range []string{"cache", "x.tmp"} {
		if _, err := os.Lstat(filepath.Join(restored, p)); !os.IsNotExist(err) {
			t.Errorf("%s should have been excluded", p)
		}
	}
}

func TestExtractPathFilter(t *testing.T) {
	src := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(src, "a"), 0o755))
	must(t, os.MkdirAll(filepath.Join(src, "b"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "a", "1"), []byte("1"), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "b", "2"), []byte("2"), 0o644))
	var buf bytes.Buffer
	_, err := Create(&buf, Options{Sources: []string{src}})
	must(t, err)

	dst := t.TempDir()
	_, err = Extract(&buf, ExtractOptions{Target: dst, Paths: []string{filepath.Join(src, "a")}})
	must(t, err)
	if _, err := os.Stat(filepath.Join(dst, src, "a", "1")); err != nil {
		t.Error("a/1 should be restored")
	}
	if _, err := os.Stat(filepath.Join(dst, src, "b")); !os.IsNotExist(err) {
		t.Error("b should not be restored")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
