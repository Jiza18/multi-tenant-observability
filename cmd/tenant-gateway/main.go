// Command tenant-gateway is the authenticating entry point to a multi-tenant
// Mimir cluster. See internal/gateway for the request flow.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Jiza18/multi-tenant-observability/internal/auth"
	"github.com/Jiza18/multi-tenant-observability/internal/gateway"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	listenAddr     string
	internalAddr   string
	upstream       string
	tokensFile     string
	reloadInterval time.Duration
	drainDelay     time.Duration
	logLevel       slog.Level
}

func main() {
	// Distroless images have no shell or curl, so the binary probes itself.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tenant-gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel}))

	upstream, err := url.Parse(cfg.upstream)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		return fmt.Errorf("invalid -upstream-url %q", cfg.upstream)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
	)
	metrics := gateway.NewMetrics(reg)

	store, err := auth.NewStore(cfg.tokensFile, logger)
	if err != nil {
		return fmt.Errorf("load tokens: %w", err)
	}
	store.OnReload = metrics.ConfigLoaded
	metrics.ConfigLoaded(store.Tokens().Len(), nil)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go store.Watch(ctx, cfg.reloadInterval)

	var draining atomic.Bool
	public := &http.Server{
		Addr: cfg.listenAddr,
		Handler: gateway.New(gateway.Config{
			Upstream: upstream,
			Tokens:   store,
			Logger:   logger,
			Metrics:  metrics,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	internal := &http.Server{
		Addr:              cfg.internalAddr,
		Handler:           internalMux(reg, &draining),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 2)
	for _, srv := range []*http.Server{public, internal} {
		go func() {
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("listen on %s: %w", srv.Addr, err)
			}
		}()
	}
	logger.Info("tenant-gateway started",
		"version", version, "listen", cfg.listenAddr, "internal", cfg.internalAddr,
		"upstream", upstream.Redacted(), "tokens", store.Tokens().Len())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Fail readiness first so load balancers stop sending traffic, then drain.
	logger.Info("shutting down", "drain_delay", cfg.drainDelay)
	draining.Store(true)
	time.Sleep(cfg.drainDelay)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return errors.Join(public.Shutdown(shutdownCtx), internal.Shutdown(shutdownCtx))
}

// healthcheck exits 0 when the local gateway reports ready. It reads the
// internal address from GATEWAY_INTERNAL_ADDRESS, like the server.
func healthcheck() int {
	_, port, err := net.SplitHostPort(env("GATEWAY_INTERNAL_ADDRESS", ":8081"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid GATEWAY_INTERNAL_ADDRESS:", err)
		return 1
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/readyz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "not ready:", resp.Status)
		return 1
	}
	return 0
}

func internalMux(reg *prometheus.Registry, draining *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	return mux
}

func parseFlags(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("tenant-gateway", flag.ContinueOnError)
	fs.StringVar(&cfg.listenAddr, "listen-address", env("GATEWAY_LISTEN_ADDRESS", ":8080"), "address for client traffic")
	fs.StringVar(&cfg.internalAddr, "internal-address", env("GATEWAY_INTERNAL_ADDRESS", ":8081"), "address for /metrics, /healthz and /readyz")
	fs.StringVar(&cfg.upstream, "upstream-url", env("GATEWAY_UPSTREAM_URL", ""), "Mimir base URL, e.g. http://mimir:8080 (required)")
	fs.StringVar(&cfg.tokensFile, "tokens-file", env("GATEWAY_TOKENS_FILE", "/etc/tenant-gateway/tokens.yaml"), "token file")
	fs.DurationVar(&cfg.reloadInterval, "reload-interval", envDuration("GATEWAY_RELOAD_INTERVAL", 10*time.Second), "how often to check the token file for changes")
	fs.DurationVar(&cfg.drainDelay, "drain-delay", envDuration("GATEWAY_DRAIN_DELAY", 5*time.Second), "time between failing readiness and closing listeners on shutdown")
	level := fs.String("log-level", env("GATEWAY_LOG_LEVEL", "info"), "debug, info, warn or error")
	showVersion := fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}
	if cfg.upstream == "" {
		return cfg, errors.New("-upstream-url (or GATEWAY_UPSTREAM_URL) is required")
	}
	if err := cfg.logLevel.UnmarshalText([]byte(*level)); err != nil {
		return cfg, fmt.Errorf("invalid -log-level: %w", err)
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return fallback
}
