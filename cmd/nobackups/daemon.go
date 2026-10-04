package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/robfig/cron/v3"

	"github.com/codemeapixel/nobackups/internal/backup"
	"github.com/codemeapixel/nobackups/internal/config"
	"github.com/codemeapixel/nobackups/internal/notify"
)

var cronParser = config.CronParser

func runDaemon(ctx context.Context, cfgPath string, verbose bool, cfg *config.Config, log *slog.Logger) error {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	var running sync.WaitGroup
	defer running.Wait()

	for {
		c, err := schedule(ctx, cfg, log, &running)
		if err != nil {
			return err
		}
		log.Info("daemon started", "version", version, "config", cfg.Path, "scheduled_jobs", len(c.Entries()))

		select {
		case <-ctx.Done():
			log.Info("shutting down; cancelling running jobs")
			<-c.Stop().Done()
			return nil
		case <-hup:
			newCfg, err := config.Load(cfgPath)
			if err != nil {
				log.Error("reload failed; keeping current config", "err", err)
			} else {
				cfg = newCfg
				log = newLogger(cfg.LogLevel, verbose)
				log.Info("config reloaded")
			}
			c.Stop()
		}
	}
}

func schedule(ctx context.Context, cfg *config.Config, log *slog.Logger, running *sync.WaitGroup) (*cron.Cron, error) {
	runner := backup.NewRunner(cfg, log)
	c := cron.New(cron.WithParser(cronParser), cron.WithChain(cron.Recover(cron.DiscardLogger)))
	for _, job := range cfg.Jobs {
		if job.Schedule == "" {
			continue
		}
		job := job
		_, err := c.AddFunc(job.Schedule, func() {
			running.Add(1)
			defer running.Done()
			res, err := runner.Run(ctx, job)
			if errors.Is(err, backup.ErrLocked) {
				log.Warn("skipping scheduled run; previous run still in progress", "job", job.Name)
				return
			}
			if res != nil {
				notify.Send(context.WithoutCancel(ctx), cfg.Notify, job, res, log)
			}
		})
		if err != nil {
			return nil, err
		}
	}
	if len(c.Entries()) == 0 {
		log.Warn("no jobs have a schedule; the daemon will idle")
	}
	c.Start()
	return c, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
