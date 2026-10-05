package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/codemeapixel/nobackups/internal/backup"
	"github.com/codemeapixel/nobackups/internal/config"
)

var client = &http.Client{Timeout: 15 * time.Second}

func Event(res *backup.Result) string {
	switch {
	case !res.Success:
		return "failure"
	case res.Warnings > 0:
		return "warning"
	default:
		return "success"
	}
}

func subscribed(on, def []string, event string) bool {
	if len(on) == 0 {
		on = def
	}
	if slices.Contains(on, event) {
		return true
	}
	return event == "warning" && slices.Contains(on, "success")
}

func Send(ctx context.Context, global config.Notify, job *config.Job, res *backup.Result, log *slog.Logger) {
	targets := []config.Notify{global}
	if job != nil {
		targets = append(targets, job.Notify)
	}
	event := Event(res)
	for _, n := range targets {
		for _, w := range n.Webhooks {
			if subscribed(w.On, []string{"failure"}, event) {
				if err := sendWebhook(ctx, w, res); err != nil {
					log.Warn("webhook notification failed", "job", res.Job, "err", err)
				}
			}
		}
		for _, d := range n.Discord {
			if subscribed(d.On, []string{"failure", "warning"}, event) {
				if err := sendDiscord(ctx, d, res); err != nil {
					log.Warn("discord notification failed", "job", res.Job, "err", err)
				}
			}
		}
	}
}

func sendWebhook(ctx context.Context, w config.Webhook, res *backup.Result) error {
	text := summary(res)
	body, err := json.Marshal(struct {
		Event   string `json:"event"`
		Text    string `json:"text"`
		Content string `json:"content"`
		*backup.Result
	}{Event(res), text, text, res})
	if err != nil {
		return err
	}
	return post(ctx, w.URL, body)
}

const (
	colorSuccess = 0x2ecc71
	colorWarning = 0xf1c40f
	colorFailure = 0xe74c3c
)

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Footer      *struct {
		Text string `json:"text"`
	} `json:"footer,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

type discordMessage struct {
	Username        string         `json:"username,omitempty"`
	Content         string         `json:"content,omitempty"`
	Embeds          []discordEmbed `json:"embeds"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

func discordPayload(d config.Discord, res *backup.Result) discordMessage {
	event := Event(res)
	e := discordEmbed{Timestamp: res.Finished.UTC().Format(time.RFC3339)}
	switch event {
	case "success":
		e.Title, e.Color = fmt.Sprintf("✅ Backup succeeded: %s", res.Job), colorSuccess
	case "warning":
		e.Title, e.Color = fmt.Sprintf("⚠️ Backup completed with warnings: %s", res.Job), colorWarning
		e.Description = fmt.Sprintf("%d file(s) could not be read and were skipped or truncated. Check the logs: `journalctl -u nobackups`", res.Warnings)
	default:
		e.Title, e.Color = fmt.Sprintf("❌ Backup failed: %s", res.Job), colorFailure
		e.Description = "```\n" + truncate(res.Error, 3900) + "\n```"
	}
	e.Fields = []discordField{
		{Name: "Host", Value: res.Host, Inline: true},
		{Name: "Duration", Value: res.Duration, Inline: true},
	}
	if res.Files > 0 {
		e.Fields = append(e.Fields,
			discordField{Name: "Files", Value: strconv.FormatInt(res.Files, 10), Inline: true},
			discordField{Name: "Size", Value: fmt.Sprintf("%s → %s stored", HumanBytes(res.Bytes), HumanBytes(res.StoredBytes)), Inline: true},
		)
	}
	if len(res.Destinations) > 0 {
		var lines []string
		for _, dr := range res.Destinations {
			if dr.Error != "" {
				lines = append(lines, fmt.Sprintf("❌ **%s**: %s", dr.Destination, truncate(dr.Error, 200)))
				continue
			}
			line := fmt.Sprintf("✅ **%s**", dr.Destination)
			if n := len(dr.Pruned); n > 0 {
				line += fmt.Sprintf(" (pruned %d old)", n)
			}
			lines = append(lines, line)
		}
		e.Fields = append(e.Fields, discordField{Name: "Destinations", Value: truncate(strings.Join(lines, "\n"), 1024)})
	}
	if len(res.Databases) > 0 {
		var lines []string
		for _, db := range res.Databases {
			if db.Error != "" {
				lines = append(lines, fmt.Sprintf("❌ **%s**: %s", db.Name, truncate(db.Error, 200)))
			} else {
				lines = append(lines, fmt.Sprintf("✅ **%s** (%s, %s)", db.Name, db.Type, HumanBytes(db.Size)))
			}
		}
		e.Fields = append(e.Fields, discordField{Name: "Databases", Value: truncate(strings.Join(lines, "\n"), 1024)})
	}
	e.Footer = &struct {
		Text string `json:"text"`
	}{"NoBackups"}

	msg := discordMessage{Username: d.Username, Embeds: []discordEmbed{e}}
	if msg.Username == "" {
		msg.Username = "NoBackups"
	}
	msg.AllowedMentions.Parse = []string{}
	if d.Mention != "" && event != "success" {
		msg.Content = d.Mention
		msg.AllowedMentions.Parse = []string{"roles", "users", "everyone"}
	}
	return msg
}

func sendDiscord(ctx context.Context, d config.Discord, res *backup.Result) error {
	body, err := json.Marshal(discordPayload(d, res))
	if err != nil {
		return err
	}
	return post(ctx, d.URL, body)
}

func post(ctx context.Context, url string, body []byte) error {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			wait := 2 * time.Second
			if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && s > 0 && s <= 10 {
				wait = time.Duration(s * float64(time.Second))
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
		}
		return nil
	}
}

func summary(r *backup.Result) string {
	switch Event(r) {
	case "success":
		return fmt.Sprintf("✅ NoBackups: job %q on %s succeeded (%d files, %s stored, %s)",
			r.Job, r.Host, r.Files, HumanBytes(r.StoredBytes), r.Duration)
	case "warning":
		return fmt.Sprintf("⚠️ NoBackups: job %q on %s completed with %d warning(s) (%s stored, %s)",
			r.Job, r.Host, r.Warnings, HumanBytes(r.StoredBytes), r.Duration)
	}
	return fmt.Sprintf("❌ NoBackups: job %q on %s failed: %s", r.Job, r.Host, r.Error)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
