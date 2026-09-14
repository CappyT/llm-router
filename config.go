package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultMaxBodyBytes = 64 << 20

// Config is the router configuration file.
type Config struct {
	Listen       string        `json:"listen"`
	DefaultRoute string        `json:"default_route"`
	MaxBodyBytes int64         `json:"max_body_bytes,omitzero"`
	Routes       []RouteConfig `json:"routes"`
}

// RouteConfig describes one upstream and the models it serves.
type RouteConfig struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
	// Models are glob patterns ("*" matches any sequence, including "/").
	Models []string `json:"models"`
	// Rewrite maps an incoming model ID to the ID sent upstream. Keys also match the route.
	Rewrite map[string]string `json:"rewrite,omitempty"`
	Auth    AuthConfig        `json:"auth"`
	// DropHeaders are removed from requests sent upstream (e.g. anthropic-beta).
	DropHeaders []string          `json:"drop_headers,omitempty"`
	SetHeaders  map[string]string `json:"set_headers,omitempty"`
	// DropFields are top-level request body fields removed on /v1/messages*.
	DropFields []string `json:"drop_fields,omitempty"`
	// DropToolFields are removed from every entry of the request "tools" array.
	DropToolFields []string `json:"drop_tool_fields,omitempty"`
	// CountTokensModel, if set, replaces the model on /v1/messages/count_tokens
	// (for upstreams whose token counting only works on some models).
	CountTokensModel string `json:"count_tokens_model,omitempty"`
	// Quota optionally polls an upstream quota endpoint and exports it as metrics.
	Quota *QuotaConfig `json:"quota,omitempty"`
}

// QuotaConfig describes an upstream quota endpoint authenticated with the route credential.
type QuotaConfig struct {
	URL      string   `json:"url"`
	Interval Duration `json:"interval,omitzero"`
}

// Duration is a time.Duration encoded as a Go duration string ("60s").
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// AuthConfig controls which credential reaches the upstream.
type AuthConfig struct {
	// Mode is one of: passthrough, bearer, x-api-key.
	Mode     string `json:"mode"`
	TokenEnv string `json:"token_env,omitempty"`
}

const (
	authPassthrough = "passthrough"
	authBearer      = "bearer"
	authXAPIKey     = "x-api-key"
)

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if len(c.Routes) == 0 {
		return errors.New("no routes defined")
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = defaultMaxBodyBytes
	}
	var errs []error
	names := map[string]bool{}
	for i, r := range c.Routes {
		if r.Name == "" {
			errs = append(errs, fmt.Errorf("routes[%d]: name is required", i))
		} else if names[r.Name] {
			errs = append(errs, fmt.Errorf("routes[%d]: duplicate name %q", i, r.Name))
		}
		names[r.Name] = true
		u, err := url.Parse(r.Upstream)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("route %q: upstream must be an absolute http(s) URL", r.Name))
		}
		switch r.Auth.Mode {
		case authPassthrough:
		case authBearer, authXAPIKey:
			if r.Auth.TokenEnv == "" {
				errs = append(errs, fmt.Errorf("route %q: auth.token_env is required for mode %q", r.Name, r.Auth.Mode))
			} else if os.Getenv(r.Auth.TokenEnv) == "" {
				errs = append(errs, fmt.Errorf("route %q: env %s is empty", r.Name, r.Auth.TokenEnv))
			}
		default:
			errs = append(errs, fmt.Errorf("route %q: unknown auth.mode %q", r.Name, r.Auth.Mode))
		}
		if q := c.Routes[i].Quota; q != nil {
			if r.Auth.Mode == authPassthrough {
				errs = append(errs, fmt.Errorf("route %q: quota polling needs a route credential (auth.mode bearer or x-api-key)", r.Name))
			}
			if u, err := url.Parse(q.URL); err != nil || u.Host == "" {
				errs = append(errs, fmt.Errorf("route %q: quota.url must be an absolute URL", r.Name))
			}
			if q.Interval.Duration <= 0 {
				q.Interval.Duration = time.Minute
			}
		}
	}
	if c.DefaultRoute == "" {
		errs = append(errs, errors.New("default_route is required"))
	} else if !names[c.DefaultRoute] {
		errs = append(errs, fmt.Errorf("default_route %q does not exist", c.DefaultRoute))
	}
	return errors.Join(errs...)
}

// globMatch reports whether s matches pattern, where "*" matches any sequence of characters.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	rest, ok := strings.CutPrefix(s, parts[0])
	if !ok {
		return false
	}
	for _, p := range parts[1 : len(parts)-1] {
		_, after, found := strings.Cut(rest, p)
		if !found {
			return false
		}
		rest = after
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}
