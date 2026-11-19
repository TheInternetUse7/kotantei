// Package app implements the Discord runtime and credential-free maintenance CLI.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"kotantei/internal/bot"
	"kotantei/internal/buildinfo"
	"kotantei/internal/config"
	"kotantei/internal/store"
	"kotantei/internal/tantei"
)

// Run returns an exit status instead of terminating the process, allowing
// maintenance commands to be exercised with real temporary databases.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, lookup func(string) (string, bool)) int {
	return run(ctx, args, stdout, stderr, lookup, bot.Run)
}

type runtimeRunner func(context.Context, *store.Outbox, config.Config, bot.RuntimeOptions) error

func run(ctx context.Context, args []string, stdout, stderr io.Writer, lookup func(string) (string, bool), runner runtimeRunner) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	fail := func(err error, code int) int {
		logger.Error("command failed", "error", err)
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return code
	}
	if err := ctx.Err(); err != nil {
		return fail(err, 130)
	}
	global := flag.NewFlagSet("kotantei", flag.ContinueOnError)
	global.SetOutput(stderr)
	configPath := global.String("config", "", "optional JSONC configuration file (plain JSON also supported)")
	global.Usage = func() { usage(stdout) }
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 64
	}
	remaining := global.Args()
	if len(remaining) == 0 || (len(remaining) == 1 && remaining[0] == "help") {
		usage(stdout)
		return 0
	}
	command := remaining[0]
	source := ""
	channel := ""
	switch command {
	case "version":
		if len(remaining) != 1 {
			return fail(errors.New("unexpected command arguments"), 64)
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(buildinfo.Current()); err != nil {
			return fail(fmt.Errorf("write build identity: %w", err), 1)
		}
		return 0
	case "run", "init", "doctor", "outbox", "health":
		if len(remaining) != 1 {
			return fail(errors.New("unexpected command arguments"), 64)
		}
	case "import":
		flags := flag.NewFlagSet("import", flag.ContinueOnError)
		flags.SetOutput(stderr)
		flags.StringVar(&source, "source", "", "Tantei SQLite source, left unchanged")
		if err := flags.Parse(remaining[1:]); err != nil {
			return 64
		}
		if strings.TrimSpace(source) == "" || len(flags.Args()) != 0 {
			return fail(errors.New("import requires --source PATH and no positional arguments"), 64)
		}
	case "resume-channel":
		flags := flag.NewFlagSet("resume-channel", flag.ContinueOnError)
		flags.SetOutput(stderr)
		flags.StringVar(&channel, "channel", "", "Discord channel whose permissions/availability have been repaired")
		if err := flags.Parse(remaining[1:]); err != nil {
			return 64
		}
		id, err := strconv.ParseUint(channel, 10, 64)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != channel || len(flags.Args()) != 0 {
			return fail(errors.New("resume-channel requires --channel ID and no positional arguments"), 64)
		}
	default:
		return fail(errors.New("unknown command; use help for available commands"), 64)
	}
	var reader io.Reader
	if *configPath != "" {
		file, err := os.Open(*configPath)
		if err != nil {
			return fail(fmt.Errorf("open configuration: %w", err), 78)
		}
		defer file.Close()
		reader = file
	}
	cfg, err := config.Load(reader, lookup)
	if err != nil {
		return fail(err, 78)
	}
	if command == "run" {
		if err := cfg.ValidateRuntime(); err != nil {
			return fail(err, 78)
		}
	}
	var summary *store.Summary
	var report *tantei.Report
	var outbox *store.DeliveryStatus
	var resumed *int
	var health *healthReport
	statusName := "ok"
	if command == "health" {
		var counts store.Health
		counts, err = store.InspectHealth(ctx, cfg.Storage.Database)
		if err == nil {
			health = &healthReport{Runtime: "unobserved", SchemaVersion: counts.SchemaVersion, Summary: counts.Summary, Outbox: counts.Outbox, Providers: []providerHealth{}}
			for _, provider := range counts.Providers {
				enabled, delay := cfg.AniList.Enabled, cfg.AniList.CycleDelay.Value()
				if provider.Name == "mal" {
					enabled, delay = cfg.MAL.Enabled, cfg.MAL.CycleDelay.Value()
				}
				health.Providers = append(health.Providers, providerHealth{ProviderHealth: provider, Enabled: enabled, CycleDelay: delay.String()})
			}
		}
	} else if command == "doctor" {
		var counts store.Summary
		counts, err = store.Inspect(ctx, cfg.Storage.Database)
		summary = &counts
	} else if command == "outbox" {
		var status store.DeliveryStatus
		status, err = store.InspectOutbox(ctx, cfg.Storage.Database)
		outbox = &status
	} else {
		if command == "resume-channel" {
			if _, err := os.Stat(cfg.Storage.Database); err != nil {
				return fail(err, 1)
			}
		}
		target, openErr := store.Open(ctx, cfg.Storage.Database)
		if openErr != nil {
			return fail(openErr, 1)
		}
		defer target.Close()
		if command == "init" && cfg.Storage.TanteiDatabase != "" {
			if _, statErr := os.Stat(cfg.Storage.TanteiDatabase); statErr == nil {
				source = cfg.Storage.TanteiDatabase
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return fail(statErr, 1)
			}
		}
		if command == "run" {
			source, err = runtimeImportSource(ctx, target, cfg, logger)
			if err != nil {
				return fail(err, 1)
			}
		}
		if source != "" {
			imported, importErr := tantei.Import(ctx, source, target)
			if importErr != nil {
				return fail(importErr, 1)
			}
			report = &imported
			if command == "run" {
				logger.Info("legacy import completed", "already_imported", imported.AlreadyImported, "users", imported.Users, "anilist_accounts", imported.AniListAccounts, "mal_accounts", imported.MALAccounts)
			}
		}
		if command == "run" {
			queue, queueErr := store.NewOutbox(target, store.Limits{})
			if queueErr != nil {
				return fail(queueErr, 1)
			}
			runtimeErr := runner(ctx, queue, cfg, bot.RuntimeOptions{Logger: logger})
			if runtimeErr != nil && !(errors.Is(runtimeErr, context.Canceled) && ctx.Err() != nil) {
				return fail(runtimeErr, 1)
			}
			statusName = "stopped"
		} else if command == "resume-channel" {
			queue, queueErr := store.NewOutbox(target, store.Limits{})
			if queueErr != nil {
				return fail(queueErr, 1)
			}
			count, resumeErr := queue.ResumeChannel(ctx, channel, time.Now().UTC())
			if resumeErr != nil {
				return fail(resumeErr, 1)
			}
			resumed = &count
			var status store.DeliveryStatus
			status, err = queue.Status(ctx)
			outbox = &status
		} else {
			var counts store.Summary
			counts, err = target.Summary(ctx)
			summary = &counts
		}
	}
	if err != nil {
		return fail(err, 1)
	}
	output := struct {
		Status  string                `json:"status"`
		Summary *store.Summary        `json:"summary,omitempty"`
		Import  *tantei.Report        `json:"import,omitempty"`
		Outbox  *store.DeliveryStatus `json:"outbox,omitempty"`
		Resumed *int                  `json:"resumed,omitempty"`
		Health  *healthReport         `json:"health,omitempty"`
	}{Status: statusName, Summary: summary, Import: report, Outbox: outbox, Resumed: resumed, Health: health}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		return fail(fmt.Errorf("write command report: %w", err), 1)
	}
	return 0
}

func usage(writer io.Writer) {
	fmt.Fprintln(writer, `Kotantei — public AniList/MyAnimeList updates for Discord

Usage: kotantei [--config FILE] COMMAND

Runtime:
  run                   Import empty storage if needed, then run the Discord bot

Credential-free maintenance commands:
  init                  Initialize storage; import configured Tantei input if present
  import --source PATH  Import Tantei into an empty destination, preserving the source
  doctor                Inspect existing Kotantei record counts, read-only
  outbox                Inspect pending/failed delivery counts and blocked channels, read-only
  health                Read-only configuration/provider/outbox aggregates; not a liveness probe
  version               Show release tag, revision, source offer, and license without configuration
  resume-channel --channel ID
                        Requeue a repaired channel's failed deliveries; do not replay sent ones
  help                  Show this help

Only run requires a Discord bot token and a MAL client ID when MAL is enabled.`)
}

type providerHealth struct {
	store.ProviderHealth
	Enabled    bool   `json:"enabled"`
	CycleDelay string `json:"cycle_delay"`
}
type healthReport struct {
	Runtime       string               `json:"runtime"`
	SchemaVersion int                  `json:"schema_version"`
	Summary       store.Summary        `json:"summary"`
	Outbox        store.DeliveryStatus `json:"outbox"`
	Providers     []providerHealth     `json:"providers"`
}

func runtimeImportSource(ctx context.Context, target *store.Store, cfg config.Config, logger *slog.Logger) (string, error) {
	if cfg.Storage.TanteiDatabase == "" {
		return "", nil
	}
	summary, err := target.Summary(ctx)
	if err != nil {
		return "", err
	}
	var imported bool
	if err := target.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM imports)`).Scan(&imported); err != nil {
		return "", err
	}
	if !imported && summary.Users+summary.Guilds+summary.Memberships+summary.Accounts+summary.Favorites != 0 {
		logger.Info("optional legacy auto-import skipped: destination already contains native data")
		return "", nil
	}
	if _, err := os.Stat(cfg.Storage.TanteiDatabase); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return cfg.Storage.TanteiDatabase, nil
}
