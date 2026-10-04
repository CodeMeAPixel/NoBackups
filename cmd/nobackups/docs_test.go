package main

import (
	"os"
	"strings"
	"testing"

	"github.com/codemeapixel/nobackups/internal/config"
)

func TestDocsConfigReferenceMatchesExample(t *testing.T) {
	doc, err := os.ReadFile("../../docs/reference/config.mdx")
	if err != nil {
		t.Skip("docs not present:", err)
	}
	if !strings.Contains(string(doc), string(exampleConfig)) {
		t.Fatal("docs/reference/config.mdx is out of date: paste cmd/nobackups/example.yaml into it")
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	for _, k := range []string{"HETZNER_ACCESS_KEY", "HETZNER_SECRET_KEY", "BACKUP_PASSPHRASE"} {
		t.Setenv(k, "x")
	}
	if _, err := config.Parse(exampleConfig, "example.yaml"); err != nil {
		t.Fatal(err)
	}
}
