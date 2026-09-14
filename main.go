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
	healthcheck := flag.Bool("healthcheck", false, "probe /livez on the admin (or listen) address and exit")
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
	// With a separate admin listener the API listener answers nothing without the client token.
	adminListen := os.Getenv("LLM_ROUTER_ADMIN_LISTEN")

	if healthcheck {
		return probe(cmp.Or(adminListen, listen))
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
	clientToken := os.Getenv("LLM_ROUTER_CLIENT_TOKEN")
	rt, err := newRouter(cfg, clientToken, transport, log, m)
	if err != nil {
		return err
	}

	var ready atomic.Bool
	apiHandler, adminHandler := newHandlers(rt, m, &ready, adminListen != "")
	servers := []*http.Server{{
		Addr:              listen,
		Handler:           apiHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}}
	if adminHandler != nil {
		servers = append(servers, &http.Server{
			Addr:              adminListen,
			Handler:           adminHandler,
			ReadHeaderTimeout: 10 * time.Second,
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	quotaClient := &http.Client{Transport: transport}
	for _, r := range rt.routes {
		if r.cfg.Quota != nil && !r.disabled {
			go m.pollQuota(ctx, r, quotaClient, log)
		}
	}

	errc := make(chan error, len(servers))
	addrs := make([]string, 0, len(servers))
	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return err
		}
		addrs = append(addrs, ln.Addr().String())
		go func() { errc <- srv.Serve(ln) }()
	}
	ready.Store(true)

	routes := make([]string, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		routes = append(routes, r.Name+"="+r.Upstream)
	}
	adminAddr := ""
	if len(addrs) > 1 {
		adminAddr = addrs[1]
	}
	log.Info("listening", "addr", addrs[0], "admin_addr", adminAddr,
		"default_route", cfg.DefaultRoute, "routes", routes, "client_token", clientToken != "")

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
	// The API listener goes first; the admin listener keeps answering probes until the end.
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	log.Info("stopped")
	return nil
}

// newHandlers returns the API handler and, when separateAdmin is set, a handler for /livez,
// /readyz and /metrics to serve on its own listener. Otherwise the API handler serves those
// endpoints too, exempt from the client token, and admin is nil.
func newHandlers(rt http.Handler, m *metrics, ready *atomic.Bool, separateAdmin bool) (api, admin http.Handler) {
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
	if separateAdmin {
		return rt, mux
	}
	mux.Handle("/", rt)
	return mux, nil
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
