package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestAdminListenerSeparation(t *testing.T) {
	cfg := twoRouteConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	m := newMetrics()
	rt, err := newRouter(cfg, "s3cret", http.DefaultTransport, slog.New(slog.DiscardHandler), m)
	if err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	ready.Store(true)

	get := func(h http.Handler, path string) (int, error) {
		srv := httptest.NewServer(h)
		defer srv.Close()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}

	api, admin := newHandlers(rt, m, &ready, true)
	for _, path := range []string{"/livez", "/readyz", "/metrics"} {
		if code, err := get(admin, path); err != nil || code != http.StatusOK {
			t.Errorf("admin %s: status %d, err %v", path, code, err)
		}
		if code, err := get(api, path); err == nil {
			t.Errorf("api %s without token answered %d, want a dropped connection", path, code)
		}
	}

	combined, admin := newHandlers(rt, m, &ready, false)
	if admin != nil {
		t.Error("admin handler returned without a separate listener")
	}
	if code, err := get(combined, "/livez"); err != nil || code != http.StatusOK {
		t.Errorf("combined /livez: status %d, err %v", code, err)
	}
}
