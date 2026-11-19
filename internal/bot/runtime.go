package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"

	"kotantei/internal/anilist"
	"kotantei/internal/config"
	"kotantei/internal/delivery"
	"kotantei/internal/mal"
	"kotantei/internal/scheduler"
	"kotantei/internal/store"
)

type RuntimeDiscord interface {
	InteractionNetwork
	CommandNetwork
	Identity(context.Context) (Identity, error)
	Register(context.Context, string, []api.CreateCommandData) error
	MemberPresent(context.Context, string, string) (bool, error)
	Connect(context.Context, Identity, func(*discord.InteractionEvent), func(), GatewayOptions) error
	Close()
}

type RuntimeOptions struct {
	Discord        RuntimeDiscord
	Gateway        GatewayOptions
	AniList        anilist.ClientOptions
	MAL            mal.ClientOptions
	Enrichment     mal.EnricherOptions
	Reconcile      scheduler.ReconcileOptions
	Delivery       delivery.Options
	StartupTimeout time.Duration
	Logger         *slog.Logger
}

type RuntimeError struct {
	Stage string
	Cause error
}

func (e *RuntimeError) Error() string { return "runtime " + e.Stage + ": " + e.Cause.Error() }
func (e *RuntimeError) Unwrap() error { return e.Cause }
func runtimeError(stage string, err error) error {
	return &RuntimeError{Stage: stage, Cause: safeError(err)}
}

type componentResult struct {
	name string
	err  error
}

// Run owns the clients/gateway/command and worker lifetimes. Its caller has
// already opened/imported storage; no Discord or provider I/O precedes that.
// A clean owner-requested restart returns nil. Parent cancellation is returned
// so the CLI can distinguish graceful runtime shutdown from maintenance errors.
func Run(parent context.Context, queue *store.Outbox, cfg config.Config, options RuntimeOptions) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if err := cfg.ValidateRuntime(); err != nil {
		return err
	}
	if queue == nil {
		return errors.New("runtime requires an initialized outbox")
	}
	if options.StartupTimeout == 0 {
		options.StartupTimeout = time.Minute
	}
	if options.StartupTimeout <= 0 {
		return errors.New("runtime startup timeout must be positive")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	logger := options.Logger
	fail := func(stage string, err error) error {
		logRuntimeFailure(logger, stage, err)
		return runtimeError(stage, err)
	}
	network := options.Discord
	if network == nil {
		client, err := NewArikawa(cfg.Discord.Token, HTTPOptions{})
		if err != nil {
			return fail("transport", err)
		}
		network = client
	}
	defer network.Close()
	identity, err := network.Identity(parent)
	if err != nil {
		return fail("identity", err)
	}
	var providers []scheduler.Provider
	commands := map[string]ProviderCommands{}
	if cfg.AniList.Enabled {
		clientOptions := options.AniList
		clientOptions.RequestsPerMinute = cfg.AniList.RequestsPerMinute
		client, err := anilist.NewClient(clientOptions)
		if err != nil {
			return fail("anilist configuration", err)
		}
		defer client.Close()
		provider, err := anilist.NewProvider(queue, client, anilist.ProviderOptions{Now: clientOptions.Now})
		if err != nil {
			return fail("anilist configuration", err)
		}
		providers = append(providers, scheduler.Provider{Name: "anilist", Delay: cfg.AniList.CycleDelay.Value(), Check: func(ctx context.Context, id int64) (scheduler.Counts, error) {
			result, err := provider.Check(ctx, id)
			return scheduler.Counts{Events: result.Events, Deliveries: result.Deliveries + result.FailedDeliveries, Warnings: result.MetadataFailures + result.FailedDeliveries}, err
		}})
		commands["anilist"] = ProviderCommands{Linker: provider, Search: func(ctx context.Context, kind, query, format string) ([]SearchResult, error) {
			items, err := client.Search(ctx, kind, query, format)
			if err != nil {
				return nil, err
			}
			result := make([]SearchResult, 0, len(items))
			for _, item := range items {
				title := item.Title.Romaji
				if title == "" {
					title = item.Title.English
				}
				if title == "" {
					title = item.Title.Native
				}
				image := item.CoverImage.ExtraLarge
				if image == "" {
					image = item.CoverImage.Large
				}
				result = append(result, SearchResult{Title: title, URL: "https://anilist.co/" + kind + "/" + strconv.FormatInt(item.ID, 10), ImageURL: image, Format: item.Format, Status: item.Status, Score: float64(item.AverageScore), Episodes: item.Episodes, Chapters: item.Chapters, Volumes: item.Volumes, Description: item.Description, Genres: item.Genres})
			}
			return result, nil
		}}
	}
	if cfg.MAL.Enabled {
		clientOptions := options.MAL
		clientOptions.ClientID = cfg.MAL.ClientID
		clientOptions.RequestInterval = cfg.MAL.RequestInterval.Value()
		client, err := mal.NewClient(clientOptions)
		if err != nil {
			return fail("mal configuration", err)
		}
		defer client.Close()
		var enricher *mal.Enricher
		if cfg.MAL.TenraiEnabled {
			enricher, err = mal.NewEnricher(options.Enrichment)
			if err != nil {
				return fail("enrichment configuration", err)
			}
			defer enricher.Close()
		}
		provider, err := mal.NewProvider(queue, client, mal.ProviderOptions{Enricher: enricher, Now: clientOptions.Now})
		if err != nil {
			return fail("mal configuration", err)
		}
		providers = append(providers, scheduler.Provider{Name: "mal", Delay: cfg.MAL.CycleDelay.Value(), Check: func(ctx context.Context, id int64) (scheduler.Counts, error) {
			result, err := provider.Check(ctx, id)
			return scheduler.Counts{Events: result.Events, Deliveries: result.Deliveries + result.FailedDeliveries, Warnings: result.ProfileFailures + result.FavoriteFailures + result.MetadataFailures + result.PrivateLists + result.FailedDeliveries}, err
		}})
		commands["mal"] = ProviderCommands{Linker: provider, Search: func(ctx context.Context, kind, query, format string) ([]SearchResult, error) {
			items, err := client.Search(ctx, kind, query, format)
			if err != nil {
				return nil, err
			}
			result := make([]SearchResult, 0, len(items))
			for _, item := range items {
				image := item.Picture.Large
				if image == "" {
					image = item.Picture.Medium
				}
				genres := make([]string, 0, len(item.Genres))
				for _, genre := range item.Genres {
					genres = append(genres, genre.Name)
				}
				result = append(result, SearchResult{Title: item.Title, URL: "https://myanimelist.net/" + kind + "/" + strconv.FormatInt(item.ID, 10), ImageURL: image, Format: item.Format, Status: item.Status, Score: item.Mean, Episodes: item.Episodes, Chapters: item.Chapters, Volumes: item.Volumes, Description: item.Synopsis, Genres: genres})
			}
			return result, nil
		}}
	}
	schedule, err := scheduler.New(queue, providers, scheduler.Options{Logger: logger, ErrorCode: providerErrorCode})
	if err != nil {
		return fail("scheduler", err)
	}
	checkMembership := func(ctx context.Context, guildID, userID string) (bool, error) {
		present, err := network.MemberPresent(ctx, guildID, userID)
		if err != nil {
			logMembershipFailure(logger, guildID, userID, err)
		}
		return present, err
	}
	reconciler, err := scheduler.NewReconciler(queue, checkMembership, options.Reconcile)
	if err != nil {
		return fail("membership", err)
	}
	options.Delivery.Logger = logger
	deliveries, err := delivery.New(queue, network, options.Delivery)
	if err != nil {
		return fail("delivery", err)
	}
	runContext, cancel := context.WithCancel(parent)
	defer cancel()
	var requestedStop atomic.Bool
	service, err := NewService(queue, network, ServiceOptions{Identity: identity, SourceURL: cfg.Project.SourceURL, Providers: commands, Scheduler: schedule, Logger: logger, Shutdown: func() { requestedStop.Store(true); cancel() }})
	if err != nil {
		return fail("commands", err)
	}
	handler, err := NewHandler(service, network, HandlerOptions{Logger: logger})
	if err != nil {
		return fail("interactions", err)
	}
	var group sync.WaitGroup
	results := make(chan componentResult, 4)
	start := func(name string, run func(context.Context) error) {
		group.Go(func() {
			err := run(runContext)
			if err == nil && runContext.Err() == nil {
				err = ErrCommandFailed
			}
			results <- componentResult{name: name, err: err}
			cancel()
		})
	}
	ready := make(chan struct{})
	var readyOnce sync.Once
	options.Gateway.Logger = logger
	start("gateway", func(ctx context.Context) error {
		return network.Connect(ctx, identity, func(event *discord.InteractionEvent) { handler.Dispatch(ctx, event) }, func() { readyOnce.Do(func() { close(ready) }) }, options.Gateway)
	})
	readyContext, cancelReady := context.WithTimeout(runContext, options.StartupTimeout)
	var startupErr error
	stage := "readiness"
	select {
	case <-ready:
		startupErr = runContext.Err()
	case <-readyContext.Done():
		startupErr = readyContext.Err()
	}
	cancelReady()
	if startupErr == nil {
		stage = "registration"
		startupErr = network.Register(runContext, identity.ApplicationID, Commands())
	}
	if startupErr == nil {
		stage = "membership"
		var result scheduler.ReconcileResult
		result, startupErr = reconciler.Cycle(runContext)
		if startupErr == nil {
			logger.Info("initial tracked membership reconciliation completed", "checked", result.Checked, "removed", result.Removed, "failures", result.Failures)
		}
	}
	if startupErr == nil && runContext.Err() == nil {
		handler.SetReady(true)
		logger.Info("runtime ready", "providers", fmt.Sprintf("anilist=%t mal=%t", cfg.AniList.Enabled, cfg.MAL.Enabled), "tenrai", cfg.MAL.Enabled && cfg.MAL.TenraiEnabled)
		start("providers", schedule.Run)
		start("delivery", deliveries.Run)
		start("membership", reconciler.Run)
		<-runContext.Done()
	}
	// Close admission before waiting. Delivery cleanup persists known receipts
	// under a short independent context, then clients/storage can safely close.
	handler.Close()
	cancel()
	group.Wait()
	handler.Wait()
	close(results)
	for result := range results {
		if result.err != nil && !errors.Is(result.err, context.Canceled) {
			return fail(result.name, result.err)
		}
	}
	if startupErr != nil && !errors.Is(startupErr, context.Canceled) {
		return fail(stage, startupErr)
	}
	if parent.Err() != nil {
		logger.Info("runtime stopped", "reason", "canceled")
		return parent.Err()
	}
	if requestedStop.Load() {
		logger.Info("runtime stopped", "reason", "owner_request")
		return nil
	}
	if startupErr != nil {
		return fail(stage, startupErr)
	}
	return fail("workers", ErrCommandFailed)
}

func providerErrorCode(err error) string {
	switch {
	case errors.Is(err, store.ErrAccountNotFound):
		return "account_removed"
	case errors.Is(err, anilist.ErrNotFound) || errors.Is(err, mal.ErrNotFound):
		return "not_found"
	case errors.Is(err, anilist.ErrNotPublic) || errors.Is(err, mal.ErrNotPublic):
		return "not_public"
	case errors.Is(err, anilist.ErrMalformed) || errors.Is(err, mal.ErrMalformed) || errors.Is(err, anilist.ErrGraphQL):
		return "malformed"
	case errors.Is(err, anilist.ErrCollectionBudget) || errors.Is(err, store.ErrPollBudget) || errors.Is(err, anilist.ErrResponseBudget) || errors.Is(err, mal.ErrResponseBudget):
		return "collection_budget"
	case errors.Is(err, anilist.ErrUnavailable) || errors.Is(err, mal.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, mal.ErrIncompleteFavorites):
		return "incomplete_favorites"
	default:
		code := errorCode(err)
		if code == "command_failed" {
			return "poll_failed"
		}
		return code
	}
}
