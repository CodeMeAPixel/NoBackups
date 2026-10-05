package dbdump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codemeapixel/nobackups/internal/config"
)

const ArchiveDir = "nobackups-databases"

type Result struct {
	Name        string
	File        string
	ArchiveName string
	Size        int64
}

type plan struct {
	argv     []string
	env      []string
	stdin    []byte
	ext      string
	writesTo string
}

const redisScript = `f=$(mktemp) || exit 1
redis-cli "$@" --rdb "$f" >/dev/null && cat "$f"
rc=$?
rm -f "$f"
exit $rc`

const mongoScript = `umask 077
f=$(mktemp) || exit 1
cat > "$f"
mongodump --config "$f" "$@"
rc=$?
rm -f "$f"
exit $rc`

const mariadbScript = `if command -v mariadb-dump >/dev/null 2>&1; then exec mariadb-dump "$@"; fi
exec mysqldump "$@"`

func buildPlan(db *config.Database, out string) (plan, error) {
	port := ""
	if db.Port > 0 {
		port = strconv.Itoa(db.Port)
	}
	var p plan
	switch db.Type {
	case "postgres":
		user := or(db.User, "postgres")
		args := []string{"-U", user}
		args = appendIf(args, "-h", db.Host)
		args = appendIf(args, "-p", port)
		if db.Database == "" {
			p.argv = append([]string{"pg_dumpall"}, args...)
			p.ext = ".sql"
		} else {
			p.argv = append(append([]string{"pg_dump"}, args...), "-Fc", "-d", db.Database)
			p.ext = ".dump"
		}
		p.argv = append(p.argv, db.Options...)
		if db.Password != "" {
			p.env = []string{"PGPASSWORD=" + db.Password}
		}
	case "mysql", "mariadb":
		args := []string{"-u", or(db.User, "root"), "--single-transaction", "--quick", "--routines", "--triggers", "--no-tablespaces"}
		args = appendIf(args, "-h", db.Host)
		args = appendIf(args, "-P", port)
		if db.Database == "" {
			args = append(args, "--all-databases", "--events")
		} else {
			args = append(args, "--databases", db.Database)
		}
		args = append(args, db.Options...)
		if db.Type == "mariadb" {
			p.argv = append([]string{"sh", "-c", mariadbScript, "sh"}, args...)
		} else {
			p.argv = append([]string{"mysqldump"}, args...)
		}
		p.ext = ".sql"
		if db.Password != "" {
			p.env = []string{"MYSQL_PWD=" + db.Password}
		}
	case "mongodb":
		args := []string{"--archive"}
		args = appendIf(args, "--host", db.Host)
		args = appendIf(args, "--port", port)
		args = appendIf(args, "--db", db.Database)
		if db.User != "" {
			args = append(args, "--username", db.User, "--authenticationDatabase", or(db.AuthDB, "admin"))
		}
		args = append(args, db.Options...)
		cfg := map[string]string{}
		if db.Password != "" {
			cfg["password"] = db.Password
		}
		stdin, err := yaml.Marshal(cfg)
		if err != nil {
			return p, err
		}
		p.argv = append([]string{"sh", "-c", mongoScript, "sh"}, args...)
		p.stdin = stdin
		p.ext = ".archive"
	case "redis":
		var args []string
		args = appendIf(args, "-h", db.Host)
		args = appendIf(args, "-p", port)
		args = appendIf(args, "--user", db.User)
		args = append(args, db.Options...)
		p.argv = append([]string{"sh", "-c", redisScript, "sh"}, args...)
		p.ext = ".rdb"
		if db.Password != "" {
			p.env = []string{"REDISCLI_AUTH=" + db.Password}
		}
	case "sqlite":
		p.ext = ".sqlite"
		p.writesTo = out + p.ext
		p.argv = []string{"sqlite3", db.Path, ".backup '" + strings.ReplaceAll(p.writesTo, "'", "''") + "'"}
	default:
		return p, fmt.Errorf("unknown database type %q", db.Type)
	}
	return p, nil
}

func wrapDocker(p plan, container string) plan {
	argv := []string{"docker", "exec", "-i"}
	for _, kv := range p.env {
		k, _, _ := strings.Cut(kv, "=")
		argv = append(argv, "-e", k)
	}
	p.argv = append(append(argv, container), p.argv...)
	return p
}

func Dump(ctx context.Context, db *config.Database, dir string) (Result, error) {
	res := Result{Name: db.Name}
	base := filepath.Join(dir, db.Name)
	p, err := buildPlan(db, base)
	if err != nil {
		return res, err
	}
	if db.Container != "" {
		id, err := ResolveContainer(ctx, db.Container)
		if err != nil {
			return res, err
		}
		p = wrapDocker(p, id)
	}
	res.File = base + p.ext
	res.ArchiveName = ArchiveDir + "/" + db.Name + p.ext

	cmd := exec.CommandContext(ctx, p.argv[0], p.argv[1:]...)
	cmd.Env = append(os.Environ(), p.env...)
	if p.stdin != nil {
		cmd.Stdin = bytes.NewReader(p.stdin)
	}
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr
	if p.writesTo == "" {
		f, err := os.OpenFile(res.File, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return res, err
		}
		cmd.Stdout = f
		err = cmd.Run()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return res, commandError(p.argv[0], err, stderr)
		}
	} else {
		cmd.Stdout = stderr
		if err := cmd.Run(); err != nil {
			return res, commandError(p.argv[0], err, stderr)
		}
		if err := os.Chmod(res.File, 0o600); err != nil {
			return res, err
		}
	}
	info, err := os.Stat(res.File)
	if err != nil {
		return res, err
	}
	if info.Size() == 0 {
		return res, fmt.Errorf("dump produced no data%s", stderrSuffix(stderr))
	}
	res.Size = info.Size()
	return res, nil
}

func ResolveContainer(ctx context.Context, name string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.ID}}\t{{.Names}}").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("docker ps: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("docker ps: %w", err)
	}
	var matches []string
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, n, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if n == name {
			return id, nil
		}
		for _, sep := range []string{".", "-", "_"} {
			if strings.HasPrefix(n, name+sep) {
				matches = append(matches, n)
				ids = append(ids, id)
				break
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no running container named %q", name)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("container %q matches several running containers (%s); use the full name", name, strings.Join(matches, ", "))
}

func commandError(tool string, err error, stderr *tailBuffer) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%s not found: install the database client tools, or set container to dump from inside a Docker container", tool)
	}
	return fmt.Errorf("%w%s", err, stderrSuffix(stderr))
}

func stderrSuffix(b *tailBuffer) string {
	s := strings.TrimSpace(b.String())
	if s == "" {
		return ""
	}
	return ": " + s
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func appendIf(args []string, flag, v string) []string {
	if v == "" {
		return args
	}
	return append(args, flag, v)
}

type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
