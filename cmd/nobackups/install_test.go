package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallIntoRoot(t *testing.T) {
	root := t.TempDir()
	o := installOptions{Root: root, BinDir: "/usr/bin", ConfigDir: "/etc/nb", UnitDir: "/lib/systemd/system"}
	if err := cmdInstall(o); err != nil {
		t.Fatal(err)
	}

	bin, err := os.Stat(filepath.Join(root, "usr/bin/nobackups"))
	if err != nil || bin.Mode().Perm() != 0o755 {
		t.Fatalf("binary: %v %v", bin, err)
	}
	for _, f := range []string{"etc/nb/config.yaml", "etc/nb/nobackups.env"} {
		fi, err := os.Stat(filepath.Join(root, f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, fi, err)
		}
	}
	unit, err := os.ReadFile(filepath.Join(root, "lib/systemd/system/nobackups.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "ExecStart=/usr/bin/nobackups -c /etc/nb/config.yaml daemon") {
		t.Errorf("unit paths not rewritten:\n%s", unit)
	}

	cfg := filepath.Join(root, "etc/nb/config.yaml")
	os.WriteFile(cfg, []byte("mine"), 0o644)
	if err := cmdInstall(o); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfg); string(b) != "mine" {
		t.Error("existing config was overwritten")
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
		t.Error("config permissions not tightened")
	}

	if err := cmdUninstall(o); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"usr/bin/nobackups", "lib/systemd/system/nobackups.service"} {
		if _, err := os.Stat(filepath.Join(root, f)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed", f)
		}
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Error("uninstall must keep the config")
	}
}
