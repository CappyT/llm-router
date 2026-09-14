package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

const sseBody = "event: message_start\n" +
	`data: {"type":"message_start","message":{"usage":{"input_tokens":1200,"cache_read_input_tokens":30000,"cache_creation_input_tokens":500,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"mentions \"usage\" in text"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n"

func TestUsageSnifferSSEAcrossChunks(t *testing.T) {
	u := &usageSniffer{sse: true}
	// Feed in awkward 7-byte chunks so lines split across writes.
	for i := 0; i < len(sseBody); i += 7 {
		u.feed([]byte(sseBody[i:min(i+7, len(sseBody))]))
	}
	got := u.finish()
	want := apiUsage{InputTokens: 1200, OutputTokens: 42, CacheReadTokens: 30000, CacheCreationTokens: 500}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestUsageSnifferLatestFieldWins(t *testing.T) {
	// Shape observed from Synthetic: message_start has the total as input,
	// message_delta resends the final input/cache split.
	body := `data: {"type":"message_start","message":{"usage":{"input_tokens":126,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":19,"output_tokens":23,"cache_creation_input_tokens":19,"cache_read_input_tokens":108}}` + "\n"
	u := &usageSniffer{sse: true}
	u.feed([]byte(body))
	want := apiUsage{InputTokens: 19, OutputTokens: 23, CacheReadTokens: 108, CacheCreationTokens: 19}
	if got := u.finish(); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestUsageSnifferJSON(t *testing.T) {
	u := &usageSniffer{}
	u.feed([]byte(`{"type":"message","usage":{"input_tokens":12,`))
	u.feed([]byte(`"output_tokens":8}}`))
	if got := u.finish(); got.InputTokens != 12 || got.OutputTokens != 8 {
		t.Errorf("got %+v", got)
	}
}

func TestMetricsEndToEnd(t *testing.T) {
	ant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseBody)
	}))
	t.Cleanup(ant.Close)

	cfg := twoRouteConfig(t, ant.URL, ant.URL)
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	m := newMetrics()
	rt, err := newRouter(cfg, "", http.DefaultTransport, slog.New(slog.DiscardHandler), m)
	if err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	handler, _ := newHandlers(rt, m, &ready, false)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp := post(t, srv.URL+"/v1/messages", `{"model":"hf:moonshotai/Kimi-K3","stream":true}`, nil)
	io.ReadAll(resp.Body)

	// The handler records metrics after the response body is flushed, so the
	// client can see EOF slightly before the counters move. requests_total is
	// updated last, so waiting on it also covers the token counters.
	requests := m.requests.WithLabelValues("synthetic", "hf:moonshotai/Kimi-K3", "200")
	deadline := time.Now().Add(2 * time.Second)
	for testutil.ToFloat64(requests) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if v := testutil.ToFloat64(requests); v != 1 {
		t.Fatalf("requests %v", v)
	}
	if v := testutil.ToFloat64(m.tokens.WithLabelValues("synthetic", "hf:moonshotai/Kimi-K3", "cache_read")); v != 30000 {
		t.Errorf("cache_read tokens %v", v)
	}
	if v := testutil.ToFloat64(m.inflight.WithLabelValues("synthetic", "hf:moonshotai/Kimi-K3")); v != 0 {
		t.Errorf("inflight %v", v)
	}

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/livez"); code != 200 {
		t.Errorf("livez %d", code)
	}
	if code, _ := get("/readyz"); code != 503 {
		t.Errorf("readyz before ready %d", code)
	}
	ready.Store(true)
	if code, _ := get("/readyz"); code != 200 {
		t.Errorf("readyz after ready %d", code)
	}
	code, body := get("/metrics")
	if code != 200 || !strings.Contains(body, `llm_router_tokens_total{model="hf:moonshotai/Kimi-K3",route="synthetic",type="output"} 42`) {
		t.Errorf("metrics %d:\n%s", code, body)
	}
}

func TestQuotaPoll(t *testing.T) {
	var auth atomic.Value
	q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		io.WriteString(w, `{"subscription":{"limit":500,"requests":12.5,"renewsAt":"2026-09-21T14:36:14.288Z","active":true,"plan":"pack"}}`)
	}))
	t.Cleanup(q.Close)

	cfg := twoRouteConfig(t, q.URL, q.URL)
	cfg.Routes[0].Quota = &QuotaConfig{URL: q.URL + "/v2/quotas"}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	m := newMetrics()
	rt, err := newRouter(cfg, "", http.DefaultTransport, slog.New(slog.DiscardHandler), m)
	if err != nil {
		t.Fatal(err)
	}
	m.scrapeQuota(t.Context(), rt.routes[0], http.DefaultClient, slog.New(slog.DiscardHandler))

	if got := auth.Load(); got != "Bearer syn-secret" {
		t.Errorf("quota auth %v", got)
	}
	checks := map[string]float64{
		"subscription.limit":    500,
		"subscription.requests": 12.5,
		"subscription.renewsAt": 1790001374,
		"subscription.active":   1,
	}
	for k, want := range checks {
		if v := testutil.ToFloat64(m.quota.WithLabelValues("synthetic", k)); v != want {
			t.Errorf("%s = %v, want %v", k, v, want)
		}
	}
}
