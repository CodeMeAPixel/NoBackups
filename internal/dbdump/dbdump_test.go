package dbdump

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/codemeapixel/nobackups/internal/config"
)

func TestPlansKeepPasswordsOffTheCommandLine(t *testing.T) {
	cases := []struct {
		db      config.Database
		tool    string
		env     string
		ext     string
		args    []string
		noArgs  []string
		stdinPw bool
	}{
		{db: config.Database{Type: "postgres", Password: "pw", Database: "app", Host: "db", Port: 5433, User: "u"},
			tool: "pg_dump", env: "PGPASSWORD=pw", ext: ".dump", args: []string{"-Fc", "-d", "app", "-h", "db", "-p", "5433", "-U", "u"}},
		{db: config.Database{Type: "postgres", Password: "pw"},
			tool: "pg_dumpall", env: "PGPASSWORD=pw", ext: ".sql", args: []string{"-U", "postgres"}},
		{db: config.Database{Type: "mysql", Password: "pw", Database: "shop"},
			tool: "mysqldump", env: "MYSQL_PWD=pw", ext: ".sql", args: []string{"--databases", "shop", "-u", "root", "--single-transaction"}, noArgs: []string{"--all-databases"}},
		{db: config.Database{Type: "mariadb", Password: "pw"},
			tool: "sh", env: "MYSQL_PWD=pw", ext: ".sql", args: []string{"--all-databases", "--events"}},
		{db: config.Database{Type: "mongodb", Password: "pw", User: "admin", Database: "app"},
			tool: "sh", ext: ".archive", args: []string{"--archive", "--username", "admin", "--authenticationDatabase", "admin", "--db", "app"}, stdinPw: true},
		{db: config.Database{Type: "redis", Password: "pw", Port: 6380},
			tool: "sh", env: "REDISCLI_AUTH=pw", ext: ".rdb", args: []string{"-p", "6380"}},
	}
	for _, c := range cases {
		p, err := buildPlan(&c.db, "/tmp/out")
		if err != nil {
			t.Fatal(err)
		}
		if p.argv[0] != c.tool || p.ext != c.ext {
			t.Errorf("%s: tool %s ext %s", c.db.Type, p.argv[0], p.ext)
		}
		for _, a := range p.argv {
			if strings.Contains(a, "pw") && !strings.Contains(a, "\n") {
				t.Errorf("%s: password on the command line: %q", c.db.Type, p.argv)
			}
		}
		if c.env != "" && !slices.Contains(p.env, c.env) {
			t.Errorf("%s: env %v, want %s", c.db.Type, p.env, c.env)
		}
		for _, a := range c.args {
			if !slices.Contains(p.argv, a) {
				t.Errorf("%s: missing %q in %q", c.db.Type, a, p.argv)
			}
		}
		for _, a := range c.noArgs {
			if slices.Contains(p.argv, a) {
				t.Errorf("%s: unexpected %q in %q", c.db.Type, a, p.argv)
			}
		}
		if c.stdinPw != strings.Contains(string(p.stdin), "password: pw") {
			t.Errorf("%s: stdin %q", c.db.Type, p.stdin)
		}
	}
}

func TestDockerWrapPassesEnvByName(t *testing.T) {
	p, _ := buildPlan(&config.Database{Type: "postgres", Password: "s3cret", Database: "app"}, "/tmp/out")
	w := wrapDocker(p, "abc123")
	want := []string{"docker", "exec", "-i", "-e", "PGPASSWORD", "abc123", "pg_dump"}
	if !slices.Equal(w.argv[:len(want)], want) {
		t.Fatalf("argv %q", w.argv)
	}
	if strings.Contains(strings.Join(w.argv, " "), "s3cret") {
		t.Fatal("password leaked into docker argv")
	}
}

func fakeDocker(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestResolveContainer(t *testing.T) {
	fakeDocker(t, `printf 'aaa\tdokploy-postgres.1.x7k2\nbbb\tapp-db-1\nccc\tapp-web-1\nddd\tapp-web-2\neee\tredis\n'`)
	ctx := context.Background()
	for name, want := range map[string]string{"dokploy-postgres": "aaa", "app-db": "bbb", "app-db-1": "bbb", "redis": "eee"} {
		got, err := ResolveContainer(ctx, name)
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := ResolveContainer(ctx, "app-web"); err == nil || !strings.Contains(err.Error(), "app-web-1, app-web-2") {
		t.Errorf("ambiguous name: %v", err)
	}
	if _, err := ResolveContainer(ctx, "mongo"); err == nil || !strings.Contains(err.Error(), "no running container") {
		t.Errorf("missing container: %v", err)
	}
}

func TestDumpThroughDocker(t *testing.T) {
	fakeDocker(t, `if [ "$1" = ps ]; then printf 'c1\tdokploy-postgres.1.abc\n'; exit 0; fi
shift 2
[ "$1" = -e ] && [ "$2" = PGPASSWORD ] || { echo "env not passed by name: $*" >&2; exit 1; }
[ "$3" = c1 ] || { echo "wrong container $3" >&2; exit 1; }
printf 'dump of %s with password %s\n' "$4" "$PGPASSWORD"`)
	dir := t.TempDir()
	db := &config.Database{Name: "panel", Type: "postgres", Container: "dokploy-postgres", User: "dokploy", Database: "dokploy", Password: "amuk"}
	res, err := Dump(context.Background(), db, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.File)
	if string(b) != "dump of pg_dump with password amuk\n" || res.ArchiveName != "nobackups-databases/panel.dump" {
		t.Fatalf("got %q as %s", b, res.ArchiveName)
	}
	if fi, _ := os.Stat(res.File); fi.Mode().Perm() != 0o600 {
		t.Errorf("dump file mode %v", fi.Mode().Perm())
	}
}

func TestDumpErrorsAreReadable(t *testing.T) {
	fakeDocker(t, `if [ "$1" = ps ]; then printf 'c1\tdb\n'; exit 0; fi
echo 'pg_dump: error: connection failed: FATAL:  password authentication failed for user "x"' >&2; exit 1`)
	_, err := Dump(context.Background(), &config.Database{Name: "x", Type: "postgres", Container: "db", Database: "x"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "password authentication failed") {
		t.Fatalf("got %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	_, err = Dump(context.Background(), &config.Database{Name: "x", Type: "postgres", Database: "x"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "pg_dump not found") {
		t.Fatalf("got %v", err)
	}
}

func TestRealDatabases(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, db *config.Database, check func(file string)) {
		t.Helper()
		res, err := Dump(ctx, db, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		check(res.File)
	}
	restoreCheck := func(t *testing.T, cmd *exec.Cmd, want string) {
		t.Helper()
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("%s: %v\n%s", cmd.Args, err, out)
		}
	}

	t.Run("postgres", func(t *testing.T) {
		port := os.Getenv("NOBACKUPS_TEST_PG_PORT")
		if port == "" {
			t.Skip("set NOBACKUPS_TEST_PG_PORT (and _PASSWORD) to test against a real PostgreSQL")
		}
		pw := os.Getenv("NOBACKUPS_TEST_PG_PASSWORD")
		db := &config.Database{Name: "pg", Type: "postgres", Host: "127.0.0.1", Port: atoi(port), Password: pw, Database: "app"}
		run(t, db, func(f string) {
			c := exec.Command("pg_restore", "-l", f)
			restoreCheck(t, c, "TABLE public t")
		})
		db.Database = ""
		run(t, db, func(f string) {
			b, _ := os.ReadFile(f)
			if !strings.Contains(string(b), "CREATE DATABASE app") || !strings.Contains(string(b), "hello") {
				t.Fatalf("pg_dumpall output missing data")
			}
		})
	})
	t.Run("mariadb", func(t *testing.T) {
		port := os.Getenv("NOBACKUPS_TEST_MYSQL_PORT")
		if port == "" {
			t.Skip("set NOBACKUPS_TEST_MYSQL_PORT, _USER and _PASSWORD to test against a real MySQL/MariaDB")
		}
		for _, typ := range []string{"mysql", "mariadb"} {
			db := &config.Database{Name: typ, Type: typ, Host: "127.0.0.1", Port: atoi(port), User: os.Getenv("NOBACKUPS_TEST_MYSQL_USER"), Password: os.Getenv("NOBACKUPS_TEST_MYSQL_PASSWORD"), Database: "shop"}
			run(t, db, func(f string) {
				b, _ := os.ReadFile(f)
				if !strings.Contains(string(b), "CREATE DATABASE") || !strings.Contains(string(b), "'apple'") {
					t.Fatalf("%s dump missing data:\n%.400s", typ, b)
				}
			})
		}
	})
	t.Run("redis", func(t *testing.T) {
		port := os.Getenv("NOBACKUPS_TEST_REDIS_PORT")
		if port == "" {
			t.Skip("set NOBACKUPS_TEST_REDIS_PORT (and _PASSWORD) to test against a real Redis")
		}
		db := &config.Database{Name: "r", Type: "redis", Host: "127.0.0.1", Port: atoi(port), Password: os.Getenv("NOBACKUPS_TEST_REDIS_PASSWORD")}
		run(t, db, func(f string) {
			b, _ := os.ReadFile(f)
			if !strings.HasPrefix(string(b), "REDIS") || !strings.Contains(string(b), "greeting") {
				t.Fatalf("not an RDB file with our key: %.40q", b)
			}
		})
	})
	t.Run("sqlite", func(t *testing.T) {
		if _, err := exec.LookPath("sqlite3"); err != nil {
			t.Skip("sqlite3 not installed")
		}
		src := filepath.Join(t.TempDir(), "app's.db")
		if out, err := exec.Command("sqlite3", src, "create table t(x); insert into t values ('kept');").CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
		run(t, &config.Database{Name: "s", Type: "sqlite", Path: src}, func(f string) {
			restoreCheck(t, exec.Command("sqlite3", f, "select x from t"), "kept")
		})
	})
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
