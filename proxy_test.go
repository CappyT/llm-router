package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type captured struct {
	path   string
	query  string
	header http.Header
	body   map[string]any
}

func upstream(t *testing.T, got chan<- captured, status int, respBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		got <- captured{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body}
		w.WriteHeader(status)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testRouter(t *testing.T, cfg *Config, clientToken string) *httptest.Server {
	t.Helper()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	rt, err := newRouter(cfg, clientToken, http.DefaultTransport, slog.New(slog.DiscardHandler), newMetrics())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func twoRouteConfig(t *testing.T, anthropicURL, syntheticURL string) *Config {
	t.Setenv("TEST_SYN_KEY", "syn-secret")
	return &Config{
		DefaultRoute: "anthropic",
		Routes: []RouteConfig{
			{
				Name:           "synthetic",
				Upstream:       syntheticURL + "/anthropic",
				Models:         []string{"hf:*", "syn:*"},
				Rewrite:        map[string]string{"kimi": "hf:moonshotai/Kimi-K3"},
				Auth:           AuthConfig{Mode: authBearer, TokenEnv: "TEST_SYN_KEY"},
				DropHeaders:    []string{"anthropic-beta"},
				DropFields:     []string{"context_management"},
				DropToolFields: []string{"defer_loading"},
			},
			{
				Name:     "anthropic",
				Upstream: anthropicURL,
				Models:   []string{"claude-*"},
				Auth:     AuthConfig{Mode: authPassthrough},
			},
		},
	}
}

func TestRouting(t *testing.T) {
	antGot := make(chan captured, 4)
	synGot := make(chan captured, 4)
	ant := upstream(t, antGot, 200, `{"ok":"anthropic"}`)
	syn := upstream(t, synGot, 200, `{"ok":"synthetic"}`)
	srv := testRouter(t, twoRouteConfig(t, ant.URL, syn.URL), "")

	oauth := map[string]string{"Authorization": "Bearer sk-ant-oat-user", "Anthropic-Beta": "oauth-2025-04-20"}

	t.Run("claude model passes through to anthropic", func(t *testing.T) {
		resp := post(t, srv.URL+"/v1/messages?beta=true", `{"model":"claude-opus-5","context_management":{"x":1}}`, oauth)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		c := <-antGot
		if c.path != "/v1/messages" || c.query != "beta=true" {
			t.Errorf("path %q query %q", c.path, c.query)
		}
		if got := c.header.Get("Authorization"); got != "Bearer sk-ant-oat-user" {
			t.Errorf("authorization %q", got)
		}
		if c.header.Get("Anthropic-Beta") == "" {
			t.Error("anthropic-beta dropped on passthrough route")
		}
		if _, ok := c.body["context_management"]; !ok {
			t.Error("context_management dropped on passthrough route")
		}
	})

	t.Run("hf model goes to synthetic with swapped credentials", func(t *testing.T) {
		body := `{"model":"hf:deepseek-ai/DeepSeek-V4.1-Flash","context_management":{},"system":[{"type":"text","text":"a<b"}],"tools":[{"name":"Bash","defer_loading":true,"input_schema":{}}]}`
		headers := map[string]string{"X-Api-Key": "leak", "X-Claude-Code-Agent-Id": "a1"}
		for k, v := range oauth {
			headers[k] = v
		}
		post(t, srv.URL+"/v1/messages?beta=true", body, headers)
		c := <-synGot
		if c.path != "/anthropic/v1/messages" {
			t.Errorf("path %q", c.path)
		}
		if got := c.header.Get("Authorization"); got != "Bearer syn-secret" {
			t.Errorf("authorization %q", got)
		}
		if c.header.Get("X-Api-Key") != "" || c.header.Get("Anthropic-Beta") != "" {
			t.Errorf("headers not dropped: %v", c.header)
		}
		if c.header.Get("X-Claude-Code-Agent-Id") != "a1" {
			t.Error("agent id header not forwarded")
		}
		if got := c.header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("accept-encoding %q, want identity", got)
		}
		if _, ok := c.body["context_management"]; ok {
			t.Error("context_management not dropped")
		}
		tool := c.body["tools"].([]any)[0].(map[string]any)
		if _, ok := tool["defer_loading"]; ok || tool["name"] != "Bash" {
			t.Errorf("tool %v", tool)
		}
		if sys := c.body["system"].([]any)[0].(map[string]any)["text"]; sys != "a<b" {
			t.Errorf("system text %v", sys)
		}
	})

	t.Run("rewrite key matches and rewrites model", func(t *testing.T) {
		post(t, srv.URL+"/v1/messages/count_tokens", `{"model":"kimi"}`, nil)
		c := <-synGot
		if c.body["model"] != "hf:moonshotai/Kimi-K3" || c.path != "/anthropic/v1/messages/count_tokens" {
			t.Errorf("model %v path %q", c.body["model"], c.path)
		}
	})

	t.Run("count_tokens_model overrides model on count_tokens only", func(t *testing.T) {
		srv := testRouter(t, func() *Config {
			c := twoRouteConfig(t, ant.URL, syn.URL)
			c.Routes[0].CountTokensModel = "hf:deepseek-ai/DeepSeek-V4.1-Flash"
			return c
		}(), "")
		post(t, srv.URL+"/v1/messages/count_tokens", `{"model":"hf:moonshotai/Kimi-K3"}`, nil)
		if c := <-synGot; c.body["model"] != "hf:deepseek-ai/DeepSeek-V4.1-Flash" {
			t.Errorf("count_tokens model %v", c.body["model"])
		}
		post(t, srv.URL+"/v1/messages", `{"model":"hf:moonshotai/Kimi-K3"}`, nil)
		if c := <-synGot; c.body["model"] != "hf:moonshotai/Kimi-K3" {
			t.Errorf("messages model %v", c.body["model"])
		}
	})

	t.Run("unknown model and non-message paths use default route", func(t *testing.T) {
		post(t, srv.URL+"/v1/messages", `{"model":"gpt-9"}`, nil)
		if c := <-antGot; c.body["model"] != "gpt-9" {
			t.Errorf("model %v", c.body["model"])
		}
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/models?limit=1000", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if c := <-antGot; c.path != "/v1/models" {
			t.Errorf("path %q", c.path)
		}
	})
}

func TestStripPrefix(t *testing.T) {
	got := make(chan captured, 1)
	up := upstream(t, got, 200, `{}`)
	t.Setenv("TEST_NANO_KEY", "nano-secret")
	cfg := twoRouteConfig(t, up.URL, up.URL)
	cfg.Routes = append([]RouteConfig{{
		Name:        "nanogpt",
		Upstream:    up.URL + "/api",
		Models:      []string{"nano:*"},
		Rewrite:     map[string]string{"nano:kimi": "moonshotai/kimi-k3"},
		StripPrefix: "nano:",
		Auth:        AuthConfig{Mode: authBearer, TokenEnv: "TEST_NANO_KEY"},
	}}, cfg.Routes...)
	srv := testRouter(t, cfg, "")

	cases := []struct{ path, model, wantPath, wantModel string }{
		{"/v1/messages", "nano:deepseek/deepseek-v4.1-flash:thinking", "/api/v1/messages", "deepseek/deepseek-v4.1-flash:thinking"},
		{"/v1/messages/count_tokens", "nano:z-ai/glm-5.3", "/api/v1/messages/count_tokens", "z-ai/glm-5.3"},
		{"/v1/messages", "nano:kimi", "/api/v1/messages", "moonshotai/kimi-k3"},
		{"/v1/messages", "claude-opus-5", "/v1/messages", "claude-opus-5"},
	}
	for _, c := range cases {
		post(t, srv.URL+c.path, `{"model":"`+c.model+`"}`, nil)
		if g := <-got; g.path != c.wantPath || g.body["model"] != c.wantModel {
			t.Errorf("%s %s: upstream path %q model %v", c.path, c.model, g.path, g.body["model"])
		}
	}
}

func TestUpstreamErrorPassthrough(t *testing.T) {
	got := make(chan captured, 1)
	errBody := `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	ant := upstream(t, got, 529, errBody)
	srv := testRouter(t, twoRouteConfig(t, ant.URL, ant.URL), "")
	resp := post(t, srv.URL+"/v1/messages", `{"model":"claude-opus-5"}`, nil)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 529 || string(raw) != errBody {
		t.Errorf("status %d body %s", resp.StatusCode, raw)
	}
}

func TestClientToken(t *testing.T) {
	got := make(chan captured, 1)
	ant := upstream(t, got, 200, `{}`)
	srv := testRouter(t, twoRouteConfig(t, ant.URL, ant.URL), "s3cret")

	if resp := post(t, srv.URL+"/v1/messages", `{"model":"claude-opus-5"}`, nil); resp.StatusCode != 401 {
		t.Errorf("missing token: status %d", resp.StatusCode)
	}
	resp := post(t, srv.URL+"/v1/messages", `{"model":"claude-opus-5"}`, map[string]string{clientTokenHeader: "s3cret"})
	if resp.StatusCode != 200 {
		t.Fatalf("valid token: status %d", resp.StatusCode)
	}
	if c := <-got; c.header.Get(clientTokenHeader) != "" {
		t.Error("client token leaked upstream")
	}
}

func TestStreamingIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	ant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: ping\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	}))
	t.Cleanup(ant.Close)
	srv := testRouter(t, twoRouteConfig(t, ant.URL, ant.URL), "")

	resp := post(t, srv.URL+"/v1/messages", `{"model":"claude-opus-5","stream":true}`, nil)
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "event: ping\n" {
			t.Errorf("first line %q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE event was buffered")
	}
	close(release)
}

func TestValidate(t *testing.T) {
	t.Setenv("EMPTY_KEY", "")
	cfg := &Config{
		DefaultRoute: "missing",
		Routes: []RouteConfig{
			{Name: "a", Upstream: "ftp://x", Auth: AuthConfig{Mode: authBearer, TokenEnv: "EMPTY_KEY"}},
			{Name: "a", Upstream: "https://x", Auth: AuthConfig{Mode: "nope"}},
		},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"duplicate name", "absolute http(s)", "EMPTY_KEY is empty", "unknown auth.mode", "default_route"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"hf:*", "hf:moonshotai/Kimi-K3", true},
		{"claude-*", "claude-opus-5", true},
		{"claude-*", "hf:claude", false},
		{"*", "anything", true},
		{"exact", "exact", true},
		{"exact", "exactly", false},
		{"a*b*c", "a-x-b-y-c", true},
		{"a*a", "a", false},
		{"*-flash", "hf:x/GLM-5.3-flash", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}
