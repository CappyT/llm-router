package main

import (
	"bytes"
	"cmp"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const clientTokenHeader = "X-Llm-Router-Token"

type router struct {
	routes      []*route
	fallback    *route
	clientToken string
	maxBody     int64
	log         *slog.Logger
	metrics     *metrics
}

type route struct {
	cfg   RouteConfig
	token string
	// disabled routes have an empty credential env; matching requests are rejected.
	disabled bool
	proxy    *httputil.ReverseProxy
}

func newRouter(cfg *Config, clientToken string, transport http.RoundTripper, log *slog.Logger, m *metrics) (*router, error) {
	rt := &router{clientToken: clientToken, maxBody: cfg.MaxBodyBytes, log: log, metrics: m}
	for _, rc := range cfg.Routes {
		target, err := url.Parse(rc.Upstream)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", rc.Name, err)
		}
		r := &route{cfg: rc}
		if rc.Auth.TokenEnv != "" {
			r.token = os.Getenv(rc.Auth.TokenEnv)
		}
		if rc.Auth.Mode != authPassthrough && r.token == "" {
			r.disabled = true
			log.Warn("route disabled: credential env is empty", "route", rc.Name, "env", rc.Auth.TokenEnv)
		}
		r.proxy = &httputil.ReverseProxy{
			Rewrite:       func(pr *httputil.ProxyRequest) { r.rewrite(pr, target) },
			Transport:     transport,
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
				log.Error("upstream error", "route", rc.Name, "path", req.URL.Path, "err", err)
				m.upstreamErrors.WithLabelValues(rc.Name).Inc()
				writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("llm-router: upstream %q: %v", rc.Name, err))
			},
		}
		rt.routes = append(rt.routes, r)
		if rc.Name == cfg.DefaultRoute {
			rt.fallback = r
		}
	}
	return rt, nil
}

func (r *route) rewrite(pr *httputil.ProxyRequest, target *url.URL) {
	pr.SetURL(target)
	h := pr.Out.Header
	switch r.cfg.Auth.Mode {
	case authBearer:
		h.Del("X-Api-Key")
		h.Set("Authorization", "Bearer "+r.token)
	case authXAPIKey:
		h.Del("Authorization")
		h.Set("X-Api-Key", r.token)
	}
	for _, name := range r.cfg.DropHeaders {
		h.Del(name)
	}
	for k, v := range r.cfg.SetHeaders {
		h.Set(k, v)
	}
}

func (r *route) matches(model string) bool {
	if _, ok := r.cfg.Rewrite[model]; ok {
		return true
	}
	return slices.ContainsFunc(r.cfg.Models, func(p string) bool { return globMatch(p, model) })
}

// transformBody applies model rewrites and field drops. The original bytes are
// returned untouched when nothing changes.
func (r *route) transformBody(body []byte, model string, countTokens bool) ([]byte, error) {
	upstreamModel, rewrite := r.cfg.Rewrite[model]
	if !rewrite && r.cfg.StripPrefix != "" {
		upstreamModel, rewrite = strings.CutPrefix(model, r.cfg.StripPrefix)
	}
	if countTokens && r.cfg.CountTokensModel != "" {
		upstreamModel, rewrite = r.cfg.CountTokensModel, true
	}
	if !rewrite && len(r.cfg.DropFields) == 0 && len(r.cfg.DropToolFields) == 0 {
		return body, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	changed := false
	if rewrite {
		v, err := json.Marshal(upstreamModel)
		if err != nil {
			return nil, err
		}
		m["model"] = v
		changed = true
	}
	for _, f := range r.cfg.DropFields {
		if _, ok := m[f]; ok {
			delete(m, f)
			changed = true
		}
	}
	if raw, ok := m["tools"]; ok && len(r.cfg.DropToolFields) > 0 {
		var tools []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("tools: %w", err)
		}
		toolsChanged := false
		for _, t := range tools {
			for _, f := range r.cfg.DropToolFields {
				if _, ok := t[f]; ok {
					delete(t, f)
					toolsChanged = true
				}
			}
		}
		if toolsChanged {
			v, err := marshalNoEscape(tools)
			if err != nil {
				return nil, err
			}
			m["tools"] = v
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	return marshalNoEscape(m)
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func (rt *router) match(model string) *route {
	for _, r := range rt.routes {
		if r.matches(model) {
			return r
		}
	}
	return rt.fallback
}

func (rt *router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if rt.clientToken != "" &&
		subtle.ConstantTimeCompare([]byte(req.Header.Get(clientTokenHeader)), []byte(rt.clientToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "authentication_error", "llm-router: invalid or missing "+clientTokenHeader)
		return
	}
	req.Header.Del(clientTokenHeader)

	start := time.Now()
	r := rt.fallback
	var model string
	isMessages := req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, "/v1/messages")
	if isMessages {
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, rt.maxBody))
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "llm-router: request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_request_error", "llm-router: read body: "+err.Error())
			return
		}
		var probe struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "llm-router: invalid JSON body: "+err.Error())
			return
		}
		model = probe.Model
		r = rt.match(model)
		if r.disabled {
			rt.log.Warn("request rejected: route disabled", "route", r.cfg.Name, "model", model, "env", r.cfg.Auth.TokenEnv)
			w.Header().Set("X-Should-Retry", "false")
			writeError(w, http.StatusServiceUnavailable, "api_error",
				fmt.Sprintf("llm-router: route %q is disabled: env %s is empty", r.cfg.Name, r.cfg.Auth.TokenEnv))
			return
		}
		if body, err = r.transformBody(body, model, req.URL.Path == "/v1/messages/count_tokens"); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "llm-router: transform body: "+err.Error())
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}

	modelLabel := cmp.Or(model, "none")
	inflight := rt.metrics.inflight.WithLabelValues(r.cfg.Name, modelLabel)
	inflight.Inc()
	rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK, start: start}
	if isMessages && req.URL.Path == "/v1/messages" {
		rec.usage = &usageSniffer{}
		// Compressed bodies can't be sniffed for usage; SSE gains little from compression anyway.
		req.Header.Set("Accept-Encoding", "identity")
	}
	r.proxy.ServeHTTP(rec, req)
	inflight.Dec()

	elapsed := time.Since(start)
	rt.metrics.requests.WithLabelValues(r.cfg.Name, modelLabel, strconv.Itoa(rec.status)).Inc()
	rt.metrics.duration.WithLabelValues(r.cfg.Name, modelLabel).Observe(elapsed.Seconds())
	if rec.wroteHeader {
		rt.metrics.ttfb.WithLabelValues(r.cfg.Name, modelLabel).Observe(rec.ttfb.Seconds())
	}
	var usage apiUsage
	if rec.usage != nil {
		usage = rec.usage.finish()
		rt.metrics.observeUsage(r.cfg.Name, modelLabel, usage)
	}

	rt.log.Info("request",
		"method", req.Method,
		"path", req.URL.Path,
		"route", r.cfg.Name,
		"model", model,
		"status", rec.status,
		"ttfb_ms", rec.ttfb.Milliseconds(),
		"duration_ms", elapsed.Milliseconds(),
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"cache_read_tokens", usage.CacheReadTokens,
		"cache_creation_tokens", usage.CacheCreationTokens,
		"agent_id", req.Header.Get("X-Claude-Code-Agent-Id"),
	)
}

// writeError emits an Anthropic-shaped error body so Claude Code can surface it.
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	})
}

// responseRecorder captures status and time to first byte, and feeds 2xx
// uncompressed bodies to the usage sniffer. Unwrap keeps flushing working.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	start       time.Time
	ttfb        time.Duration
	usage       *usageSniffer
}

func (s *responseRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
		s.ttfb = time.Since(s.start)
		if s.usage != nil {
			h := s.Header()
			if code < 200 || code >= 300 || h.Get("Content-Encoding") != "" {
				s.usage = nil
			} else {
				s.usage.sse = strings.HasPrefix(h.Get("Content-Type"), "text/event-stream")
			}
		}
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *responseRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	if s.usage != nil {
		s.usage.feed(b)
	}
	return s.ResponseWriter.Write(b)
}

func (s *responseRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
