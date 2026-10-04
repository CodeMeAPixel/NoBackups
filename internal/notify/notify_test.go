package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codemeapixel/nobackups/internal/backup"
	"github.com/codemeapixel/nobackups/internal/config"
)

func TestSubscribed(t *testing.T) {
	def := []string{"failure", "warning"}
	cases := []struct {
		on    []string
		event string
		want  bool
	}{
		{nil, "failure", true},
		{nil, "warning", true},
		{nil, "success", false},
		{[]string{"success"}, "warning", true}, // success subscribers also get warnings
		{[]string{"failure"}, "warning", false},
		{[]string{"success", "failure"}, "success", true},
	}
	for _, c := range cases {
		if got := subscribed(c.on, def, c.event); got != c.want {
			t.Errorf("subscribed(%v, %q) = %v, want %v", c.on, c.event, got, c.want)
		}
	}
}

func TestDiscordPayload(t *testing.T) {
	res := &backup.Result{
		Job: "db", Host: "web01", Success: false, Error: "upload failed for: hetzner",
		Finished: time.Now(), Duration: "3s", Files: 10, Bytes: 2048, StoredBytes: 1024,
		Destinations: []backup.DestResult{
			{Destination: "hetzner", Error: "connection refused"},
			{Destination: "local", Key: "k", Pruned: []string{"a", "b"}},
		},
	}
	msg := discordPayload(config.Discord{Mention: "<@&123>"}, res)
	e := msg.Embeds[0]
	if e.Color != colorFailure || !strings.Contains(e.Title, "failed") || !strings.Contains(e.Description, "upload failed") {
		t.Errorf("failure embed wrong: %+v", e)
	}
	if msg.Content != "<@&123>" || len(msg.AllowedMentions.Parse) == 0 {
		t.Errorf("mention not set: %+v", msg)
	}
	dest := e.Fields[len(e.Fields)-1].Value
	if !strings.Contains(dest, "❌ **hetzner**: connection refused") || !strings.Contains(dest, "pruned 2 old") {
		t.Errorf("destinations field: %q", dest)
	}

	res.Success, res.Error = true, ""
	msg = discordPayload(config.Discord{Mention: "<@&123>"}, res)
	if msg.Embeds[0].Color != colorSuccess || msg.Content != "" {
		t.Errorf("success should be green and not ping: %+v", msg)
	}
	res.Warnings = 2
	if msg = discordPayload(config.Discord{}, res); msg.Embeds[0].Color != colorWarning {
		t.Errorf("warning colour wrong")
	}
}

func TestSendRoutesGlobalAndJobTargets(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got[r.URL.Path]++
		mu.Unlock()
		if r.URL.Path == "/limited" && got[r.URL.Path] == 1 {
			w.Header().Set("Retry-After", "0.1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	global := config.Notify{
		Discord:  []config.Discord{{URL: srv.URL + "/global-discord"}},                       // default: failure, warning
		Webhooks: []config.Webhook{{URL: srv.URL + "/global-hook", On: []string{"success"}}}, // success only
	}
	job := &config.Job{Name: "j", Notify: config.Notify{
		Discord: []config.Discord{{URL: srv.URL + "/limited", On: []string{"failure"}}},
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	Send(context.Background(), global, job, &backup.Result{Job: "j", Success: false, Error: "boom"}, log)
	Send(context.Background(), global, job, &backup.Result{Job: "j", Success: true}, log)

	want := map[string]int{"/global-discord": 1, "/global-hook": 1, "/limited": 2 /* 429 then retry */}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s called %d times, want %d (all: %v)", k, got[k], v, got)
		}
	}
}
