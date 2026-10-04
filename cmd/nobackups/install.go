package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/codemeapixel/nobackups/deploy"
)

type installOptions struct {
	Root      string
	BinDir    string
	ConfigDir string
	UnitDir   string
	NoSystemd bool
}

func (o installOptions) path(p string) string { return filepath.Join(o.Root, p) }

func (o installOptions) systemd() bool {
	if o.Root != "" || o.NoSystemd {
		return false
	}
	_, err := exec.LookPath("systemctl")
	return err == nil
}

func cmdInstall(o installOptions) error {
	if o.Root == "" && os.Geteuid() != 0 {
		return errors.New("install needs root: run it with sudo")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}

	binPath := filepath.Join(o.BinDir, "nobackups")
	dst := o.path(binPath)
	if err := installBinary(self, dst); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	fmt.Println("installed", dst)

	cfgDir := o.path(o.ConfigDir)
	if err := os.MkdirAll(cfgDir, 0o750); err != nil {
		return err
	}
	cfgFile := filepath.Join(cfgDir, "config.yaml")
	envFile := filepath.Join(cfgDir, "nobackups.env")
	for _, f := range []struct {
		path string
		data []byte
	}{{cfgFile, exampleConfig}, {envFile, deploy.EnvExample}} {
		if _, err := os.Stat(f.path); err == nil {
			fmt.Println("keeping existing", f.path)
		} else {
			if err := os.WriteFile(f.path, f.data, 0o600); err != nil {
				return err
			}
			fmt.Println("created", f.path)
		}
		if err := os.Chmod(f.path, 0o600); err != nil {
			return err
		}
	}

	unitDir := o.path(o.UnitDir)
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unit := strings.NewReplacer(
		"/usr/local/bin/nobackups", binPath,
		"/etc/nobackups/", strings.TrimSuffix(o.ConfigDir, "/")+"/",
	).Replace(deploy.SystemdUnit)
	unitFile := filepath.Join(unitDir, "nobackups.service")
	if err := os.WriteFile(unitFile, []byte(unit), 0o644); err != nil {
		return err
	}
	fmt.Println("installed", unitFile)

	restarted := false
	if o.systemd() {
		if err := systemctl("daemon-reload"); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not reload systemd:", err)
		} else if systemctl("is-active", "--quiet", "nobackups") == nil {
			if err := systemctl("restart", "nobackups"); err != nil {
				fmt.Fprintln(os.Stderr, "warning: could not restart nobackups:", err)
			} else {
				restarted = true
			}
		}
	}

	fmt.Printf("\nNoBackups %s installed.\n", version)
	if restarted {
		fmt.Println("The running service was restarted on the new version.")
		return nil
	}
	if o.Root == "" {
		fmt.Printf(`
Next steps:
  1. Edit %[1]s and %[2]s
  2. nobackups validate && nobackups check
  3. nobackups run --all
  4. systemctl enable --now nobackups
`, filepath.Join(o.ConfigDir, "config.yaml"), filepath.Join(o.ConfigDir, "nobackups.env"))
	}
	return nil
}

func installBinary(src, dst string) error {
	if si, err := os.Stat(src); err == nil {
		if di, err := os.Stat(dst); err == nil && os.SameFile(si, di) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func cmdUninstall(o installOptions) error {
	if o.Root == "" && os.Geteuid() != 0 {
		return errors.New("uninstall needs root: run it with sudo")
	}
	if o.systemd() {
		_ = systemctl("disable", "--now", "nobackups")
	}
	for _, p := range []string{filepath.Join(o.BinDir, "nobackups"), filepath.Join(o.UnitDir, "nobackups.service")} {
		if err := os.Remove(o.path(p)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Println("removed", o.path(p))
	}
	if o.systemd() {
		_ = systemctl("daemon-reload")
	}
	fmt.Printf("Config (%s), state and backups were left in place.\n", o.ConfigDir)
	return nil
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
