package config

import (
	"strings"
	"testing"
)

const minimal = `
destinations:
  s3:
    type: s3
    endpoint: http://localhost:9000/
    bucket: b
    access_key_id: ${NB_TEST_KEY}
    secret_access_key: ${NB_TEST_SECRET:-fallback}
    prefix: /srv/{hostname}/
jobs:
  - name: etc
    sources: [/etc]
    destinations: [s3]
    schedule: "@daily"
`

func TestParseExpandsEnvAndDefaults(t *testing.T) {
	t.Setenv("NB_TEST_KEY", "key123")
	cfg, err := Parse([]byte("hostname: web01\n"+minimal), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Destinations["s3"]
	if d.AccessKeyID != "key123" || d.SecretAccessKey != "fallback" {
		t.Errorf("env expansion: got %q / %q", d.AccessKeyID, d.SecretAccessKey)
	}
	if d.Endpoint != "localhost:9000" || *d.UseSSL {
		t.Errorf("endpoint normalisation: got %q ssl=%v", d.Endpoint, *d.UseSSL)
	}
	if d.Prefix != "srv/web01" {
		t.Errorf("prefix: got %q", d.Prefix)
	}
	if cfg.Jobs[0].Compression != "zstd" || d.PartSizeMB != 64 {
		t.Errorf("defaults not applied")
	}
}

func TestParseMissingEnv(t *testing.T) {
	_, err := Parse([]byte(minimal), "test.yaml")
	if err == nil || !strings.Contains(err.Error(), "NB_TEST_KEY") {
		t.Fatalf("expected missing variable error, got %v", err)
	}
}

func TestEnvInCommentsIgnored(t *testing.T) {
	t.Setenv("NB_TEST_KEY", "k")
	if _, err := Parse([]byte("# ${NOT_SET_ANYWHERE}\n"+minimal), "test.yaml"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateErrors(t *testing.T) {
	cfg := `
destinations:
  bad name:
    type: ftp
jobs:
  - name: a
    sources: [relative/path]
    destinations: [nope]
    schedule: "not a cron"
    compression: lz4
    encryption: {passphrase: x, recipients: [y]}
    notify:
      discord: [{url: "http://insecure", on: [sometimes]}]
  - name: a
    sources: [/x]
    destinations: [nope]
`
	_, err := Parse([]byte(cfg), "test.yaml")
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"unknown type", "may only contain", "absolute path", "unknown destination",
		"invalid schedule", "compression must be", "exactly one of", "duplicate job name",
		"https:// Discord webhook URL", `unknown event "sometimes"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing error %q in:\n%v", want, err)
		}
	}
}

func TestUnquotedBraceValueHint(t *testing.T) {
	cfg := strings.Replace(minimal, "prefix: /srv/{hostname}/", "prefix: {hostname}", 1)
	t.Setenv("NB_TEST_KEY", "k")
	_, err := Parse([]byte(cfg), "test.yaml")
	if err == nil || !strings.Contains(err.Error(), `prefix: "{hostname}"`) {
		t.Fatalf("expected a quoting hint, got %v", err)
	}
	quoted := strings.Replace(minimal, "prefix: /srv/{hostname}/", `prefix: "{hostname}"`, 1)
	c, err := Parse([]byte("hostname: web01\n"+quoted), "test.yaml")
	if err != nil || c.Destinations["s3"].Prefix != "web01" {
		t.Fatalf("quoted prefix: %v %+v", err, c)
	}
}

func TestProviderShorthands(t *testing.T) {
	cfg := `
destinations:
  alarik: {type: alarik, endpoint: cdn.example.com, bucket: b, access_key_id: k, secret_access_key: s}
  hetzner: {type: hetzner, region: nbg1, bucket: b, access_key_id: k, secret_access_key: s}
  aws: {type: aws, region: eu-west-1, bucket: b, access_key_id: k, secret_access_key: s}
  minio: {type: minio, endpoint: "http://10.0.0.5:9000", path_style: false, bucket: b, access_key_id: k, secret_access_key: s}
  custom: {type: hetzner, region: fsn1, endpoint: s3.internal, bucket: b, access_key_id: k, secret_access_key: s}
jobs:
  - name: j
    sources: [/etc]
    destinations: [alarik]
`
	c, err := Parse([]byte(cfg), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		endpoint, region string
		pathStyle        bool
	}{
		"alarik":  {"cdn.example.com", "us-east-1", true},
		"hetzner": {"nbg1.your-objectstorage.com", "nbg1", false},
		"aws":     {"s3.eu-west-1.amazonaws.com", "eu-west-1", false},
		"minio":   {"10.0.0.5:9000", "us-east-1", false},
		"custom":  {"s3.internal", "fsn1", false},
	}
	for name, w := range want {
		d := c.Destinations[name]
		if d.Type != "s3" || d.Provider == "" || d.Endpoint != w.endpoint || d.Region != w.region || *d.PathStyle != w.pathStyle {
			t.Errorf("%s: type=%s provider=%s endpoint=%s region=%s path_style=%v", name, d.Type, d.Provider, d.Endpoint, d.Region, *d.PathStyle)
		}
	}
	if *c.Destinations["minio"].UseSSL {
		t.Error("http:// endpoint should disable TLS for provider types too")
	}
}

func TestProviderNeedsRegion(t *testing.T) {
	cfg := `
destinations:
  h: {type: hetzner, bucket: b, access_key_id: k, secret_access_key: s}
  x: {type: dropbox}
jobs: [{name: j, sources: [/etc], destinations: [h]}]
`
	_, err := Parse([]byte(cfg), "test.yaml")
	if err == nil || !strings.Contains(err.Error(), "region is required for hetzner (e.g. fsn1)") || !strings.Contains(err.Error(), "alarik, aws, b2") {
		t.Fatalf("got %v", err)
	}
}

func TestDatabaseValidation(t *testing.T) {
	good := `
destinations: {d: {type: local, path: /tmp/x}}
jobs:
  - name: j
    destinations: [d]
    databases:
      - {type: postgres, database: app}
      - {type: postgres}
      - {type: sqlite, path: /srv/data/app.db}
      - {type: redis, container: cache}
`
	c, err := Parse([]byte(good), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, db := range c.Jobs[0].Databases {
		names = append(names, db.Name)
	}
	if strings.Join(names, ",") != "postgres-app,postgres,sqlite-app,redis" {
		t.Errorf("default names: %v", names)
	}

	bad := `
destinations: {d: {type: local, path: /tmp/x}}
jobs:
  - name: j
    destinations: [d]
    databases:
      - {type: oracle}
      - {type: sqlite, container: x}
      - {type: redis, database: "0"}
      - {type: mysql, database: shop}
      - {type: mysql, database: shop}
      - {type: postgres, port: 70000}
  - name: empty
    destinations: [d]
`
	_, err = Parse([]byte(bad), "test.yaml")
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"type must be one of postgres", "sqlite needs an absolute path", "sqlite is dumped from the host",
		"redis dumps the whole server", `duplicate name "mysql-shop"`, "invalid port 70000", "at least one source or database"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}
