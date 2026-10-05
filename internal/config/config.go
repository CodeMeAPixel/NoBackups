package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

var CronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

const (
	DefaultPath     = "/etc/nobackups/config.yaml"
	DefaultStateDir = "/var/lib/nobackups"
)

type Config struct {
	StateDir     string                  `yaml:"state_dir"`
	LogLevel     string                  `yaml:"log_level"`
	Hostname     string                  `yaml:"hostname"`
	Destinations map[string]*Destination `yaml:"destinations"`
	Jobs         []*Job                  `yaml:"jobs"`
	Notify       Notify                  `yaml:"notify"`
	Path         string                  `yaml:"-"`
}

type Destination struct {
	Name string `yaml:"-"`
	Type string `yaml:"type"`

	// s3
	Endpoint           string `yaml:"endpoint"`
	Region             string `yaml:"region"`
	Bucket             string `yaml:"bucket"`
	AccessKeyID        string `yaml:"access_key_id"`
	SecretAccessKey    string `yaml:"secret_access_key"`
	SessionToken       string `yaml:"session_token"`
	UseSSL             *bool  `yaml:"use_ssl"`
	PathStyle          bool   `yaml:"path_style"`
	StorageClass       string `yaml:"storage_class"`
	PartSizeMB         int    `yaml:"part_size_mb"`
	CAFile             string `yaml:"ca_file"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`

	Path string `yaml:"path"`

	Prefix string `yaml:"prefix"`
}

type Job struct {
	Name             string      `yaml:"name"`
	Sources          []string    `yaml:"sources"`
	Exclude          []string    `yaml:"exclude"`
	Destinations     []string    `yaml:"destinations"`
	Schedule         string      `yaml:"schedule"`
	Compression      string      `yaml:"compression"`
	CompressionLevel int         `yaml:"compression_level"`
	Encryption       *Encryption `yaml:"encryption"`
	Retention        Retention   `yaml:"retention"`
	Hooks            Hooks       `yaml:"hooks"`
	Timeout          Duration    `yaml:"timeout"`
	Notify           Notify      `yaml:"notify"`
	OneFileSystem    bool        `yaml:"one_file_system"`
}

type Encryption struct {
	Passphrase     string   `yaml:"passphrase"`
	PassphraseFile string   `yaml:"passphrase_file"`
	Recipients     []string `yaml:"recipients"`
	IdentityFile   string   `yaml:"identity_file"`
}

type Retention struct {
	KeepLast int `yaml:"keep_last"`
	KeepDays int `yaml:"keep_days"`
}

type Hooks struct {
	Before []string `yaml:"before"`
	After  []string `yaml:"after"`
}

type Notify struct {
	Webhooks []Webhook `yaml:"webhooks"`
	Discord  []Discord `yaml:"discord"`
}

var NotifyEvents = []string{"success", "warning", "failure"}

type Webhook struct {
	URL string   `yaml:"url"`
	On  []string `yaml:"on"`
}

type Discord struct {
	URL      string   `yaml:"url"`
	On       []string `yaml:"on"`
	Mention  string   `yaml:"mention"`
	Username string   `yaml:"username"`
}

func (n *Notify) validate(where string, add func(string, ...any)) {
	checkOn := func(kind string, i int, on []string) {
		for _, e := range on {
			if !slices.Contains(NotifyEvents, e) {
				add("%s: %s #%d: unknown event %q (expected success, warning or failure)", where, kind, i+1, e)
			}
		}
	}
	for i, w := range n.Webhooks {
		if !strings.HasPrefix(w.URL, "http://") && !strings.HasPrefix(w.URL, "https://") {
			add("%s: webhook #%d: url must start with http:// or https://", where, i+1)
		}
		checkOn("webhook", i, w.On)
	}
	for i, d := range n.Discord {
		if !strings.HasPrefix(d.URL, "https://") {
			add("%s: discord #%d: url must be a https:// Discord webhook URL", where, i+1)
		}
		checkOn("discord", i, d.On)
	}
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Value == "" {
		return nil
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	d.Duration = v
	return nil
}

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	envRe  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)
)

func Load(path string) (*Config, error) {
	if err := loadEnvFile(filepath.Join(filepath.Dir(path), "nobackups.env")); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw, path)
}

func Parse(raw []byte, path string) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var missing []string
	expandNode(&root, &missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("undefined environment variables referenced in config: %s", strings.Join(uniq(missing), ", "))
	}
	cfg := &Config{}
	if root.Kind != 0 {
		if err := root.Decode(cfg); err != nil {
			if strings.Contains(err.Error(), "cannot unmarshal !!map into string") {
				return nil, fmt.Errorf("parse %s: %w\nhint: a value starting with { must be quoted, e.g. prefix: \"{hostname}\"", path, err)
			}
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	cfg.Path = path
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func expandNode(n *yaml.Node, missing *[]string) {
	if n.Kind == yaml.ScalarNode && n.Tag != "!!null" {
		n.Value = envRe.ReplaceAllStringFunc(n.Value, func(m string) string {
			sub := envRe.FindStringSubmatch(m)
			if v, ok := os.LookupEnv(sub[1]); ok {
				return v
			}
			if strings.Contains(m, ":-") {
				return sub[2]
			}
			*missing = append(*missing, sub[1])
			return ""
		})
	}
	for _, c := range n.Content {
		expandNode(c, missing)
	}
}

func (c *Config) applyDefaults() {
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Hostname == "" {
		c.Hostname, _ = os.Hostname()
	}
	for name, d := range c.Destinations {
		if d == nil {
			continue
		}
		d.Name = name
		d.Prefix = strings.Trim(strings.ReplaceAll(d.Prefix, "{hostname}", c.Hostname), "/")
		if d.Type == "s3" {
			if strings.HasPrefix(d.Endpoint, "http://") {
				d.Endpoint = strings.TrimPrefix(d.Endpoint, "http://")
				if d.UseSSL == nil {
					f := false
					d.UseSSL = &f
				}
			}
			d.Endpoint = strings.TrimSuffix(strings.TrimPrefix(d.Endpoint, "https://"), "/")
			if d.UseSSL == nil {
				t := true
				d.UseSSL = &t
			}
			if d.PartSizeMB == 0 {
				d.PartSizeMB = 64
			}
			if d.Region == "" {
				d.Region = "us-east-1"
			}
		}
	}
	for _, j := range c.Jobs {
		if j == nil {
			continue
		}
		if j.Compression == "" {
			j.Compression = "zstd"
		}
	}
}

func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if len(c.Destinations) == 0 {
		add("no destinations defined")
	}
	for name, d := range c.Destinations {
		if d == nil {
			add("destination %q is empty", name)
			continue
		}
		if !nameRe.MatchString(name) {
			add("destination %q: name may only contain letters, digits, '.', '_' and '-'", name)
		}
		switch d.Type {
		case "s3":
			if d.Endpoint == "" {
				add("destination %q: endpoint is required", name)
			}
			if d.Bucket == "" {
				add("destination %q: bucket is required", name)
			}
			if d.AccessKeyID == "" || d.SecretAccessKey == "" {
				add("destination %q: access_key_id and secret_access_key are required", name)
			}
			if d.PartSizeMB < 5 {
				add("destination %q: part_size_mb must be at least 5", name)
			}
		case "local":
			if d.Path == "" {
				add("destination %q: path is required", name)
			} else if !filepath.IsAbs(d.Path) {
				add("destination %q: path must be absolute", name)
			}
		case "":
			add("destination %q: type is required (s3 or local)", name)
		default:
			add("destination %q: unknown type %q (expected s3 or local)", name, d.Type)
		}
	}

	c.Notify.validate("notify", add)

	if len(c.Jobs) == 0 {
		add("no jobs defined")
	}
	seen := map[string]bool{}
	for i, j := range c.Jobs {
		if j == nil {
			add("job #%d is empty", i+1)
			continue
		}
		id := j.Name
		if id == "" {
			id = fmt.Sprintf("#%d", i+1)
			add("job %s: name is required", id)
		} else if !nameRe.MatchString(j.Name) {
			add("job %q: name may only contain letters, digits, '.', '_' and '-'", j.Name)
		}
		if seen[j.Name] {
			add("job %q: duplicate job name", j.Name)
		}
		seen[j.Name] = true
		if len(j.Sources) == 0 {
			add("job %s: at least one source is required", id)
		}
		for _, s := range j.Sources {
			if !filepath.IsAbs(s) {
				add("job %s: source %q must be an absolute path", id, s)
			}
		}
		for _, p := range j.Exclude {
			if _, err := filepath.Match(p, ""); err != nil {
				add("job %s: invalid exclude pattern %q", id, p)
			}
		}
		if len(j.Destinations) == 0 {
			add("job %s: at least one destination is required", id)
		}
		for _, d := range j.Destinations {
			if _, ok := c.Destinations[d]; !ok {
				add("job %s: unknown destination %q", id, d)
			}
		}
		if j.Schedule != "" {
			if _, err := CronParser.Parse(j.Schedule); err != nil {
				add("job %s: invalid schedule %q: %v", id, j.Schedule, err)
			}
		}
		switch j.Compression {
		case "zstd", "gzip", "none":
		default:
			add("job %s: compression must be zstd, gzip or none", id)
		}
		if j.Retention.KeepLast < 0 || j.Retention.KeepDays < 0 {
			add("job %s: retention values must not be negative", id)
		}
		j.Notify.validate("job "+id+": notify", add)
		if e := j.Encryption; e != nil {
			n := 0
			for _, set := range []bool{e.Passphrase != "", e.PassphraseFile != "", len(e.Recipients) > 0} {
				if set {
					n++
				}
			}
			if n != 1 {
				add("job %s: encryption needs exactly one of passphrase, passphrase_file or recipients", id)
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Config) Job(name string) (*Job, bool) {
	for _, j := range c.Jobs {
		if j.Name == name {
			return j, true
		}
	}
	return nil, false
}

func (c *Config) DestinationNames() []string {
	names := make([]string, 0, len(c.Destinations))
	for n := range c.Destinations {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func uniq(s []string) []string {
	sort.Strings(s)
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
