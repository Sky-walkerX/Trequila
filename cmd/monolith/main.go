// Command monolith runs every Trequila service in one process: the public API, the
// planner, discovery and router workers, and the agent endpoints. The services talk to
// each other only through the event bus, so any of them can later be started in its own
// process with the same code.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/bus/natsbus"
	"github.com/sky-walkerx/trequila/internal/config"
	"github.com/sky-walkerx/trequila/internal/crypto"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/images"
	"github.com/sky-walkerx/trequila/internal/logging"
	"github.com/sky-walkerx/trequila/internal/middleware"
	"github.com/sky-walkerx/trequila/internal/outbox"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/svc/agent"
	"github.com/sky-walkerx/trequila/internal/svc/api"
	"github.com/sky-walkerx/trequila/internal/svc/discovery"
	"github.com/sky-walkerx/trequila/internal/svc/planner"
	"github.com/sky-walkerx/trequila/internal/svc/router"
	"github.com/sky-walkerx/trequila/internal/tripevents"
	"github.com/sky-walkerx/trequila/internal/trips"
	"golang.org/x/time/rate"
)

func main() {
	cfg := config.Load()
	log := logging.Setup(cfg.LogLevel, cfg.LogFormat)

	if err := run(cfg, log); err != nil {
		log.Error("trequila stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting trequila")

	cipher, err := crypto.NewCipherFromBase64(cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("invalid ENCRYPTION_KEY: %w", err)
	}

	db, err := openDB(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer db.Close()

	nb, err := natsbus.Connect(ctx, natsbus.Config{
		URL: cfg.NATSURL, CredsFile: cfg.NATSCredsFile, CredsContent: cfg.NATSCreds, Logger: log,
	})
	if err != nil {
		return err
	}
	defer nb.Close()

	// Shared collaborators.
	authSvc := auth.NewAuthService(db)
	keys := providerkeys.NewStore(db, cipher)
	relay := &outbox.Relay{DB: db, Pub: nb, Log: log}
	tripStore := &trips.Store{DB: db, Keys: keys, Relay: relay}
	hub := tripevents.NewHub(cfg.DatabaseURL, log)
	geoSvc := geo.NewService(cfg.RedisAddr)
	imgSvc := images.NewService(cfg.RedisAddr)

	// Services.
	apiSvc := api.New(api.Deps{
		DB: db, Auth: authSvc, Keys: keys, Trips: tripStore, Hub: hub, Relay: relay,
		Pinger: nb, FrontendURL: cfg.FrontendURL, Log: log,
	})
	agentSvc := agent.New(agent.Deps{DB: db, Auth: authSvc, Keys: keys, Trips: tripStore, Geo: geoSvc, Images: imgSvc})

	mux := http.NewServeMux()
	apiSvc.Routes(mux)
	agentSvc.Routes(mux)

	var subs []bus.Subscription
	subs = append(subs, apiSvc.Subscriptions()...)
	subs = append(subs, planner.New(keys, nb, planner.WithLogger(log)).Subscriptions()...)
	subs = append(subs, discovery.New(geoSvc, imgSvc, nb).WithLogger(log).Subscriptions()...)
	subs = append(subs, router.New(geoSvc, nb).WithLogger(log).Subscriptions()...)
	subs = append(subs, agentSvc.Subscriptions()...)

	var wg sync.WaitGroup
	go_ := func(name string, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
			log.Debug("stopped", "component", name)
		}()
	}

	go_("trip-event-hub", func() { hub.Run(ctx) })
	go_("outbox-relay", func() { relay.Run(ctx, 10*time.Second) })
	go_("api-jobs", func() { apiSvc.Run(ctx) })
	for _, s := range subs {
		s.Handler = bus.Chain(s.Handler, bus.Recover(log), bus.Logging(log, s.Durable), bus.Metrics(s.Durable))
		go_(s.Durable, func() {
			if err := nb.Subscribe(ctx, s); err != nil {
				log.Error("subscription ended", "durable", s.Durable, "error", err)
			}
		})
	}

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: middleware.CORS(middleware.DefaultCORSConfig())(middleware.NewRateLimiter(rate.Limit(10), 20).Limit(mux)),
		// No WriteTimeout: it is absolute from the start of the response, so any value
		// would cap how long an SSE stream may run. The streaming handlers enforce
		// their own deadlines and send heartbeats; other handlers return promptly.
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	if os.Getenv("ENABLE_KEEP_ALIVE") == "true" {
		go_("keep-alive", func() { keepAlive(ctx, cfg.Port, log) })
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		if err != nil {
			stop()
			wg.Wait()
			return fmt.Errorf("http server: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http server did not shut down cleanly", "error", err)
	}
	wg.Wait()
	log.Info("trequila stopped")
	return nil
}

func openDB(ctx context.Context, url string, log *slog.Logger) (*sql.DB, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	for i := 1; i <= 30; i++ {
		if err = db.PingContext(ctx); err == nil {
			log.Info("connected to postgres")
			return db, nil
		}
		log.Info("waiting for postgres", "attempt", i, "error", err)
		select {
		case <-ctx.Done():
			db.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	db.Close()
	return nil, fmt.Errorf("postgres not reachable: %w", err)
}

// keepAlive pings the process's own liveness endpoint so a dyno that idles out on a
// free hosting plan stays awake.
func keepAlive(ctx context.Context, port string, log *slog.Logger) {
	t := time.NewTicker(25 * time.Minute)
	defer t.Stop()
	url := fmt.Sprintf("http://localhost:%s/ping", port)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			resp, err := http.Get(url)
			if err != nil {
				log.Warn("keep-alive ping failed", "error", err)
				continue
			}
			resp.Body.Close()
		}
	}
}
