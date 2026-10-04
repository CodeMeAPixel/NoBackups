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
