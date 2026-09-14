package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const maxJSONUsageBody = 4 << 20

type metrics struct {
	registry       *prometheus.Registry
	requests       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	ttfb           *prometheus.HistogramVec
	inflight       *prometheus.GaugeVec
	tokens         *prometheus.CounterVec
	upstreamErrors *prometheus.CounterVec
	quota          *prometheus.GaugeVec
	quotaScrapes   *prometheus.CounterVec
	rejected       prometheus.Counter
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	m := &metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_router_requests_total",
			Help: "Proxied requests by route, model and upstream status code.",
		}, []string{"route", "model", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "llm_router_request_duration_seconds",
			Help:    "Full request duration including the streamed response body.",
			Buckets: prometheus.ExponentialBuckets(0.25, 2, 13),
		}, []string{"route", "model"}),
		ttfb: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "llm_router_time_to_first_byte_seconds",
			Help:    "Time until upstream response headers; includes upstream queueing.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 14),
		}, []string{"route", "model"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "llm_router_inflight_requests",
			Help: "Requests currently being proxied.",
		}, []string{"route", "model"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_router_tokens_total",
			Help: "Tokens reported in upstream usage blocks (type: input, output, cache_read, cache_creation).",
		}, []string{"route", "model", "type"}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_router_upstream_errors_total",
			Help: "Transport-level failures reaching the upstream.",
		}, []string{"route"}),
		quota: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "llm_router_upstream_quota",
			Help: "Numeric leaves of the upstream quota endpoint response (timestamps as unix seconds).",
		}, []string{"route", "key"}),
		quotaScrapes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_router_quota_scrapes_total",
			Help: "Quota endpoint polls by result.",
		}, []string{"route", "result"}),
		rejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "llm_router_rejected_requests_total",
			Help: "Requests dropped without a response for a missing or wrong client token.",
		}),
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.duration, m.ttfb, m.inflight, m.tokens, m.upstreamErrors, m.quota, m.quotaScrapes, m.rejected,
	)
	return m
}

type apiUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

// rawUsage keeps track of which fields an event actually carried.
type rawUsage struct {
	InputTokens         *int64 `json:"input_tokens"`
	OutputTokens        *int64 `json:"output_tokens"`
	CacheReadTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens *int64 `json:"cache_creation_input_tokens"`
}

// usageSniffer extracts usage from a /v1/messages response without buffering
// streamed bodies. SSE usage values are cumulative, so the latest event that
// carries a field wins: Anthropic's message_delta usually has only
// output_tokens, while some providers (e.g. Synthetic) resend the final
// input/cache split there.
type usageSniffer struct {
	sse      bool
	buf      []byte
	overflow bool
	usage    apiUsage
}

func (u *usageSniffer) feed(b []byte) {
	if u.overflow {
		return
	}
	u.buf = append(u.buf, b...)
	if !u.sse {
		if len(u.buf) > maxJSONUsageBody {
			u.overflow, u.buf = true, nil
		}
		return
	}
	rest := u.buf
	for {
		line, after, ok := bytes.Cut(rest, []byte("\n"))
		if !ok {
			break
		}
		u.line(line)
		rest = after
	}
	u.buf = append(u.buf[:0], rest...)
	if len(u.buf) > maxJSONUsageBody {
		u.buf = u.buf[:0]
	}
}

func (u *usageSniffer) line(l []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(l, "\r"), []byte("data:"))
	if !ok || !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	u.parse(data)
}

func (u *usageSniffer) parse(data []byte) {
	var ev struct {
		Usage   *rawUsage `json:"usage"`
		Message *struct {
			Usage *rawUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return
	}
	u.merge(ev.Usage)
	if ev.Message != nil {
		u.merge(ev.Message.Usage)
	}
}

func (u *usageSniffer) merge(v *rawUsage) {
	if v == nil {
		return
	}
	set := func(dst *int64, src *int64) {
		if src != nil {
			*dst = *src
		}
	}
	set(&u.usage.InputTokens, v.InputTokens)
	set(&u.usage.OutputTokens, v.OutputTokens)
	set(&u.usage.CacheReadTokens, v.CacheReadTokens)
	set(&u.usage.CacheCreationTokens, v.CacheCreationTokens)
}

func (u *usageSniffer) finish() apiUsage {
	if !u.sse && !u.overflow && len(u.buf) > 0 {
		u.parse(u.buf)
	}
	return u.usage
}

func (m *metrics) observeUsage(route, model string, u apiUsage) {
	for typ, n := range map[string]int64{
		"input":          u.InputTokens,
		"output":         u.OutputTokens,
		"cache_read":     u.CacheReadTokens,
		"cache_creation": u.CacheCreationTokens,
	} {
		if n > 0 {
			m.tokens.WithLabelValues(route, model, typ).Add(float64(n))
		}
	}
}

// pollQuota periodically fetches a quota endpoint and exports its numeric leaves.
func (m *metrics) pollQuota(ctx context.Context, r *route, client *http.Client, log *slog.Logger) {
	m.scrapeQuota(ctx, r, client, log)
	tick := time.Tick(r.cfg.Quota.Interval.Duration)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			m.scrapeQuota(ctx, r, client, log)
		}
	}
}

func (m *metrics) scrapeQuota(ctx context.Context, r *route, client *http.Client, log *slog.Logger) {
	values, err := fetchQuota(ctx, client, r.cfg.Quota.URL, r)
	if err != nil {
		m.quotaScrapes.WithLabelValues(r.cfg.Name, "error").Inc()
		log.Warn("quota poll failed", "route", r.cfg.Name, "err", err)
		return
	}
	m.quota.DeletePartialMatch(prometheus.Labels{"route": r.cfg.Name})
	for k, v := range values {
		m.quota.WithLabelValues(r.cfg.Name, k).Set(v)
	}
	m.quotaScrapes.WithLabelValues(r.cfg.Name, "success").Inc()
}

func fetchQuota(ctx context.Context, client *http.Client, url string, r *route) (map[string]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	switch r.cfg.Auth.Mode {
	case authBearer:
		req.Header.Set("Authorization", "Bearer "+r.token)
	case authXAPIKey:
		req.Header.Set("X-Api-Key", r.token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	flattenNumeric("", doc, out)
	return out, nil
}

func flattenNumeric(prefix string, v any, out map[string]float64) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			flattenNumeric(joinKey(prefix, k), child, out)
		}
	case []any:
		for i, child := range t {
			flattenNumeric(joinKey(prefix, fmt.Sprint(i)), child, out)
		}
	case float64:
		out[prefix] = t
	case bool:
		out[prefix] = map[bool]float64{false: 0, true: 1}[t]
	case string:
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			out[prefix] = float64(ts.Unix())
		}
	}
}

func joinKey(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return strings.Join([]string{prefix, k}, ".")
}
