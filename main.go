package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	configPath := flag.String("config", cmp.Or(os.Getenv("LLM_ROUTER_CONFIG"), "/etc/llm-router/config.json"), "path to config file")
	healthcheck := flag.Bool("healthcheck", false, "probe /livez on the configured listen address and exit")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*configPath, *healthcheck, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, healthcheck bool, log *slog.Logger) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	listen := cmp.Or(os.Getenv("LLM_ROUTER_LISTEN"), cfg.Listen, ":8787")

	if healthcheck {
		return probe(listen)
	}

	drainDelay := 5 * time.Second
	if v := os.Getenv("LLM_ROUTER_DRAIN_DELAY"); v != "" {
		if drainDelay, err = time.ParseDuration(v); err != nil {
			return fmt.Errorf("LLM_ROUTER_DRAIN_DELAY: %w", err)
		}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 32

	m := newMetrics()
	rt, err := newRouter(cfg, os.Getenv("LLM_ROUTER_CLIENT_TOKEN"), transport, log, m)
	if err != nil {
		return err
	}

	var ready atomic.Bool
	srv := &http.Server{
		Addr:              listen,
		Handler:           newHandler(rt, m, &ready),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	quotaClient := &http.Client{Transport: transport}
	for _, r := range rt.routes {
		if r.cfg.Quota != nil {
			go m.pollQuota(ctx, r, quotaClient, log)
		}
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	ready.Store(true)

	routes := make([]string, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		routes = append(routes, r.Name+"="+r.Upstream)
	}
	log.Info("listening", "addr", ln.Addr().String(), "default_route", cfg.DefaultRoute, "routes", routes,
		"client_token", os.Getenv("LLM_ROUTER_CLIENT_TOKEN") != "")

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Fail readiness first so endpoints are removed before listeners close.
	ready.Store(false)
	log.Info("draining", "delay", drainDelay.String())
	time.Sleep(drainDelay)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.Info("stopped")
	return nil
}

func newHandler(rt http.Handler, m *metrics, ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	mux.Handle("/", rt)
	return mux
}

func probe(listen string) error {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/livez")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("livez returned %d", resp.StatusCode)
	}
	return nil
}
