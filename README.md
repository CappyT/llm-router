# llm-router

A small reverse proxy that lets a single Claude Code session route requests to different
Anthropic-compatible backends based on the `model` field. The typical use case: orchestrate with
Opus on a Claude subscription and delegate subagent work to open models on
[Synthetic](https://synthetic.new) or [NanoGPT](https://nano-gpt.com).

Claude Code has one global `ANTHROPIC_BASE_URL` per process, so per-model backends need a proxy.
Every backend here already speaks the Anthropic Messages API, so the router does **no format
translation**: it picks an upstream, swaps credentials, optionally strips headers/fields, and streams
the response through unbuffered.

```
claude (claude.ai login)
  ANTHROPIC_BASE_URL=http://127.0.0.1:8787
        │
    llm-router ──── model claude-*        → api.anthropic.com            (Authorization passed through)
               ├─── model hf:* / syn:*    → api.synthetic.new/anthropic  (Authorization replaced, anthropic-beta dropped)
               ├─── model nano:*          → nano-gpt.com/api             (Authorization replaced, nano: prefix stripped)
               └─── anything else         → default_route
```

## Features

- Routing by model glob (`hf:*`, `claude-*`), optional model ID rewrite (`"kimi": "hf:moonshotai/Kimi-K3"`)
  or routing-prefix stripping (`nano:z-ai/glm-5.3` → `z-ai/glm-5.3`).
- Per-route auth: `passthrough`, `bearer`, `x-api-key`. The subscription OAuth token never reaches
  routes that replace auth.
- Routes whose credential env is empty are disabled instead of blocking startup, so one config serves
  any subset of providers.
- Per-route header drop/set, top-level body field drop, per-tool field drop.
- SSE streaming with immediate flush; upstream error bodies are passed through unchanged (Claude Code
  matches on error wording for retries).
- Prometheus `/metrics`: requests, duration, time-to-first-byte, in-flight, token usage (including
  cache read/creation) parsed from response `usage` blocks, and upstream quota polling.
- `/livez`, `/readyz` with graceful drain on SIGTERM.
- Optional shared client token (`X-Llm-Router-Token`): requests without it get no response at all. With
  `LLM_ROUTER_ADMIN_LISTEN`, probes and metrics move to their own port, so the API port can face the
  internet.
- Stdlib + `prometheus/client_golang` only, distroless static image.

## Quick start

Published images: `ghcr.io/cappyt/llm-router:<version>` (`linux/amd64`, `linux/arm64`).

```bash
docker run -d --name llm-router -p 127.0.0.1:8787:8787 -e SYNTHETIC_API_KEY ghcr.io/cappyt/llm-router:0.2.0
curl -s localhost:8787/readyz
```

Or build locally with Compose:

```bash
export SYNTHETIC_API_KEY=... NANOGPT_API_KEY=...   # either or both
docker compose up -d --build
```

The image ships `config.example.json` as `/etc/llm-router/config.json`; mount your own to override.
Set the keys of the providers you use: routes whose key is empty are disabled, and the default
`anthropic` route needs none.

## Claude Code setup

Point Claude Code at the router and keep your claude.ai login. Do **not** set `ANTHROPIC_AUTH_TOKEN`
or `ANTHROPIC_API_KEY`: when only `ANTHROPIC_BASE_URL` is set, the saved subscription login stays the
active credential and is forwarded to the `anthropic` route.

```bash
claude-mix() {
  ANTHROPIC_BASE_URL=http://127.0.0.1:8787 \
  CLAUDE_CODE_ATTRIBUTION_HEADER=0 \
  claude "$@"
}
```

Or persist it in `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787",
    "CLAUDE_CODE_ATTRIBUTION_HEADER": "0"
  }
}
```

`CLAUDE_CODE_ATTRIBUTION_HEADER=0` keeps the system prompt prefix byte-stable, which Synthetic
recommends to avoid cache busting.

If you set `LLM_ROUTER_CLIENT_TOKEN`, add
`ANTHROPIC_CUSTOM_HEADERS="X-Llm-Router-Token: <token>"`. With a missing or wrong token the router
drops the connection, so Claude Code reports a connection error rather than a 401.

### Subagents on Synthetic

Define agents with a Synthetic model in the frontmatter; the main loop keeps using Opus and picks the
agent from its `description`. Three tiers are provided in [`examples/agents/`](examples/agents/) —
copy them to `~/.claude/agents/` or `<project>/.claude/agents/`:

| Agent | Model | Use |
|---|---|---|
| `worker-small` | `hf:zai-org/GLM-5.3-Flash` | Exploration, lookups, summaries, trivial edits (~0.1 request each). |
| `worker` (default) | `hf:deepseek-ai/DeepSeek-V4.1-Flash` | Default for scoped code changes and tests. |
| `worker-big` | `hf:moonshotai/Kimi-K3` | Hard refactors/debugging, image input (1 request each). |

```yaml
---
name: worker
description: Default implementation worker (DeepSeek-V4.1-Flash) for well-scoped coding tasks ...
model: hf:deepseek-ai/DeepSeek-V4.1-Flash
tools: Read, Grep, Glob, Bash, Edit, Write
---
```

Claude Code accepts arbitrary model IDs in agent frontmatter; it logs an `unrecognized_model` warning
and sends the ID verbatim. Optionally set `CLAUDE_CODE_SUBAGENT_MODEL=hf:deepseek-ai/DeepSeek-V4.1-Flash`
to also send built-in agents without a `model` field (Explore, general-purpose) to Synthetic.

Keep the `tools:` allowlist: without it a subagent inherits every tool including all MCP servers
(observed with Claude Code 2.1.270: 57 tool schemas without it, 4 with it), and those schemas are resent on every request.

Tips specific to Synthetic's subscription limits:

- **Concurrency is 1 request per model** (extra requests are queued, not rejected). The three tiers use
  three different models, so they run in parallel; two `worker` agents at once will queue. Watch
  `llm_router_time_to_first_byte_seconds` to spot queueing.
- **Requests are scaled by model input price**: Kimi-K3 counts as 1 request, GLM-5.3-Flash as ~0.1.
- The agents pin `hf:` IDs because each tier wants a specific model. Synthetic rotates models and
  pinned IDs return 404 once removed; the `syn:` aliases track replacements automatically
  (`syn:large:text` → GLM-5.3-Flash, `syn:large:vision` → Kimi-K3; DeepSeek has no alias).
- `count_tokens` and `/v2/quotas` calls do not count against limits.

### Subagents on NanoGPT

NanoGPT model IDs (`vendor/model`, plus a few bare names) have no prefix that sets them apart, so the
example route claims a `nano:` prefix and strips it before forwarding:

```json
{
  "name": "nanogpt",
  "upstream": "https://nano-gpt.com/api",
  "models": ["nano:*"],
  "strip_prefix": "nano:",
  "auth": { "mode": "bearer", "token_env": "NANOGPT_API_KEY" }
}
```

In agent frontmatter use `model: nano:<NanoGPT ID>`, e.g. `nano:deepseek/deepseek-v4.1-flash`,
`nano:z-ai/glm-5.3-flash` or `nano:moonshotai/kimi-k3`. Suffixes pass through unchanged
(`nano:deepseek/deepseek-v4-pro:thinking`); IDs are case-sensitive.

- The upstream is `https://nano-gpt.com/api` (Messages at `/api/v1/messages`). NanoGPT's own Claude
  Code guide uses `…/api/v1` as base URL, which makes clients call `/api/v1/v1/messages` and follow a
  307 redirect.
- List models with context length, capabilities, pricing and subscription inclusion:
  `curl -s 'https://nano-gpt.com/api/v1/models?detailed=true' | jq '.data[] | {id, context_length, pricing, subscription}'`.
  `/api/subscription/v1/models` lists only the subscription models.
- The subscription covers open models only (no `anthropic/*` model is included; some count input
  tokens 2x). Per NanoGPT's docs billing is picked per request, and `"set_headers": {"X-Billing-Mode": "paygo"}`
  forces pay-as-you-go. Subscribers can add `"quota": {"url": "https://nano-gpt.com/api/subscription/v1/usage"}`
  to export `weeklyInputTokens.remaining`, `weeklyInputTokens.percentUsed`, `active`,
  `routing.subscriptionQuotaAvailable` and related keys (`weeklyInputTokens.resetAt` is epoch
  milliseconds).
- NanoGPT's `count_tokens` is a generic estimate that ignores the model: for a small request with a
  system prompt and one tool it returned 82 tokens where DeepSeek-V4.1-Flash billed 339, so `/context`
  is approximate for `nano:` models.
- Per NanoGPT's docs, non-Claude models are served by converting the request to OpenAI chat format
  upstream; tool use and thinking fidelity depend on the model.

### What was verified

Tested on 2026-09-14 with Claude Code 2.1.270 and a real Synthetic subscription: Opus on the claude.ai
login orchestrated `worker-small`, `worker` and `worker-big` in parallel through the router.

- The subscription OAuth token passes through to `api.anthropic.com`; the Opus prompt cache works
  (~46k tokens cache read per turn).
- The three subagents ran concurrently on GLM-5.3-Flash, DeepSeek-V4.1-Flash and Kimi-K3, used tools
  (Write, Bash) correctly, and Opus reproduced their results.
- With the `tools:` allowlist a subagent request is ~2k input tokens. Synthetic's prefix cache hits on
  follow-up turns (`cache_read_input_tokens` ≈ previous prompt).
- Synthetic streams proper Anthropic `tool_use` / `input_json_delta` events and returns `thinking`
  blocks.
- Synthetic's `POST /anthropic/v1/messages/count_tokens` returns **500 for Kimi-K3 and GLM-5.3-Flash**
  (even for a plain string message) and 200 for DeepSeek-V4.1-Flash and GLM-4.7-Flash. The example
  config sets `count_tokens_model` so counting goes through DeepSeek (different tokenizer, close enough
  for `/context`).

NanoGPT was tested on 2026-09-14 with Claude Code 2.1.270 (headless, no user settings) and a
pay-as-you-go key. The main loop ran on `nano:deepseek/deepseek-v4.1-flash`, a project subagent on
`nano:z-ai/glm-5.3-flash`, and `ANTHROPIC_DEFAULT_HAIKU_MODEL` also pointed at GLM-5.3-Flash.

- All 10 `/v1/messages` requests returned 200 through the router with Claude Code's own
  `anthropic-beta`, `context_management`, `cache_control` and tool fields, so the route needs no
  `drop_headers` or `drop_fields`.
- The main loop spawned the subagent with the Agent tool. The subagent used Bash, Write and Read, its
  requests carried `x-claude-code-agent-id`, and the main loop read back the file it wrote.
- Streamed tool calls arrive as `tool_use` / `input_json_delta` events. `message_start` reports zero
  usage and `message_delta` the final counts, which the usage metrics pick up.
- NanoGPT's prefix cache hits on follow-up turns (`cache_read_input_tokens` ≈ 19k on DeepSeek, 2–3k
  on GLM); `cache_creation_input_tokens` stayed 0.
- Time to first byte was 4–13 s per request, around 8 s on GLM-5.3-Flash.
- `count_tokens` through the router (`nano:` stripped) returned 200.
- With `NANOGPT_API_KEY` unset the route is disabled: Claude Code received the 503 once and exited
  after 2 s without retrying.

## Configuration

Config file path: `-config` flag, `LLM_ROUTER_CONFIG`, default `/etc/llm-router/config.json`.
Unknown fields are rejected. Routes are evaluated in order; the first match wins, unmatched models and
non-`/v1/messages*` paths go to `default_route`.

| Field | Description |
|---|---|
| `listen` | Listen address, default `:8787` (env `LLM_ROUTER_LISTEN` overrides). |
| `default_route` | Route name used for unmatched models and other paths (e.g. `/v1/models`). |
| `max_body_bytes` | Request body limit, default 64 MiB. |
| `routes[].name` | Unique name, used in metrics and logs. |
| `routes[].upstream` | Base URL; the request path is appended (`…/anthropic` + `/v1/messages`). |
| `routes[].models` | Glob patterns; `*` matches any sequence including `/`. |
| `routes[].rewrite` | Map incoming model ID → upstream model ID. Keys also match the route. |
| `routes[].strip_prefix` | Prefix removed from the model ID sent upstream when present (e.g. `nano:`). `rewrite` wins; the route still needs a matching `models` glob. |
| `routes[].auth.mode` | `passthrough`, `bearer` or `x-api-key`. |
| `routes[].auth.token_env` | Env var holding the credential (required unless passthrough). If it is empty at startup the route is disabled: a warning is logged, quota polling is skipped and matching requests get 503 with `x-should-retry: false`. The default route must have it. |
| `routes[].drop_headers` | Request headers removed before forwarding. |
| `routes[].set_headers` | Request headers set before forwarding. |
| `routes[].drop_fields` | Top-level body fields removed on `/v1/messages*` (e.g. `context_management`). |
| `routes[].drop_tool_fields` | Fields removed from every `tools[]` entry (e.g. `defer_loading`). |
| `routes[].count_tokens_model` | Model used instead of the requested one on `/v1/messages/count_tokens`. |
| `routes[].quota.url` | Optional quota endpoint polled with the route credential. |
| `routes[].quota.interval` | Poll interval, Go duration, default `60s`. |

Environment:

| Variable | Description |
|---|---|
| `LLM_ROUTER_CONFIG` | Config file path. |
| `LLM_ROUTER_LISTEN` | Overrides `listen`. |
| `LLM_ROUTER_CLIENT_TOKEN` | If set, API requests must carry `X-Llm-Router-Token`. Requests without it, or with a wrong value, get no response at all: the connection is closed (HTTP/2: the stream is reset) and `llm_router_rejected_requests_total` is incremented. The token is compared as SHA-256 digests in constant time and stripped before forwarding. Probes and metrics stay exempt unless `LLM_ROUTER_ADMIN_LISTEN` is set. |
| `LLM_ROUTER_ADMIN_LISTEN` | Serves `/livez`, `/readyz` and `/metrics` on this address (e.g. `:9090`) instead of the API listener, which then answers nothing without the client token. `-healthcheck` probes this address. |
| `LLM_ROUTER_DRAIN_DELAY` | Time between failing `/readyz` and closing listeners on SIGTERM, default `5s`. |

The body is only re-encoded when a rewrite or field drop actually applies; otherwise upstream receives
the original bytes.

## Endpoints

| Path | Description |
|---|---|
| `GET /livez` | 200 while the process serves HTTP. |
| `GET /readyz` | 200 once listening, 503 after SIGTERM (drain). Does not probe upstreams, to avoid cascading failures. |
| `GET /metrics` | Prometheus exposition. |
| everything else | Proxied. |

All three are served on `LLM_ROUTER_ADMIN_LISTEN` instead when it is set.

### Exposing the router publicly

Set a long random `LLM_ROUTER_CLIENT_TOKEN` (e.g. `openssl rand -hex 32`) and
`LLM_ROUTER_ADMIN_LISTEN`, point probes and scraping at the admin port, and route only the API port
through the gateway. A reverse proxy in front still answers on its own when the router drops a
connection (typically with a 502/503), so rate-limit unauthenticated traffic there as well.

## Metrics

| Metric | Labels | Notes |
|---|---|---|
| `llm_router_requests_total` | `route`, `model`, `code` | Upstream status code (502 for transport errors). |
| `llm_router_request_duration_seconds` | `route`, `model` | Includes the full streamed body. |
| `llm_router_time_to_first_byte_seconds` | `route`, `model` | Time to response headers; includes upstream queueing. |
| `llm_router_inflight_requests` | `route`, `model` | |
| `llm_router_tokens_total` | `route`, `model`, `type` | `input`, `output`, `cache_read`, `cache_creation`, from `usage` blocks of `/v1/messages` 2xx responses (SSE and JSON). For SSE the latest event carrying a field wins. Values are reported as the upstream sends them: Synthetic reports `input_tokens` equal to `cache_creation_input_tokens` (the uncached part counted in both), so don't add the two for Synthetic. NanoGPT's `input_tokens` excludes `cache_read_input_tokens`. |
| `llm_router_upstream_errors_total` | `route` | Transport-level failures. |
| `llm_router_upstream_quota` | `route`, `key` | Numeric leaves of the quota response, flattened with dots (`subscription.requests`); RFC 3339 strings become unix seconds, booleans 0/1. |
| `llm_router_quota_scrapes_total` | `route`, `result` | |
| `llm_router_rejected_requests_total` | | Requests dropped for a missing or wrong client token. |

Synthetic's `/v2/quotas` currently exposes (among others) `subscription.limit`,
`subscription.requests`, `rollingFiveHourLimit.remaining`, `rollingFiveHourLimit.max`,
`rollingFiveHourLimit.limited`, `weeklyTokenLimit.percentRemaining`, `weeklyTokenLimit.nextRegenAt`
and `search.hourly.*`. The schema is undocumented and may change; the router exports whatever numeric
leaves it finds.

Go runtime and process collectors are included. `model` is the model ID as sent by the client
(before rewrite); its cardinality is bounded by what your clients send.

Useful queries:

```promql
# cache hit ratio per route
sum by (route) (rate(llm_router_tokens_total{type="cache_read"}[1h]))
  / sum by (route) (rate(llm_router_tokens_total{type=~"input|cache_read|cache_creation"}[1h]))

# Synthetic 5h request budget left
llm_router_upstream_quota{route="synthetic",key="rollingFiveHourLimit.remaining"}

# p90 queueing/latency before first byte per model
histogram_quantile(0.9, sum by (le, model) (rate(llm_router_time_to_first_byte_seconds_bucket[15m])))
```

Access logs are JSON on stdout, one line per request, with route, model, status, TTFB, duration,
token usage and `x-claude-code-agent-id` (set on subagent requests).

## Kubernetes

Run it as a Deployment with the config in a ConfigMap and the key in a Secret:

```yaml
containers:
  - name: llm-router
    image: ghcr.io/cappyt/llm-router:0.2.0
    ports: [{ name: http, containerPort: 8787 }]
    env:
      - name: SYNTHETIC_API_KEY
        valueFrom: { secretKeyRef: { name: llm-router, key: synthetic-api-key, optional: true } }
      - name: NANOGPT_API_KEY
        valueFrom: { secretKeyRef: { name: llm-router, key: nanogpt-api-key, optional: true } }
      - name: LLM_ROUTER_CLIENT_TOKEN
        valueFrom: { secretKeyRef: { name: llm-router, key: client-token } }
    volumeMounts:
      - { name: config, mountPath: /etc/llm-router, readOnly: true }
    livenessProbe:  { httpGet: { path: /livez, port: http } }
    readinessProbe: { httpGet: { path: /readyz, port: http }, periodSeconds: 2 }
    securityContext:
      runAsNonRoot: true
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities: { drop: [ALL] }
terminationGracePeriodSeconds: 90
```

Scrape `/metrics` on the `http` port with a ServiceMonitor/PodMonitor. When the router is reachable
beyond localhost, always set `LLM_ROUTER_CLIENT_TOKEN`: otherwise anyone who can reach it spends your
provider keys.

## Caveats

- Anthropic does not support Claude Code on non-Claude models; subagent quality depends entirely on
  the model.
- Because `ANTHROPIC_BASE_URL` points to a non-Anthropic host, Claude Code runs in gateway mode for the
  whole session (e.g. some first-party-only features may be disabled for the Opus loop too).
- Claude Code does not know the context window of `hf:`/`syn:`/`nano:` IDs. Check with `/context` inside a
  subagent-heavy session; `CLAUDE_CODE_MAX_CONTEXT_TOKENS` is global and would also affect Opus.
- Synthetic's documented Messages schema does not list `thinking`, `metadata`, `cache_control` or
  beta fields. Claude Code works against it directly per Synthetic's own guide; if a specific field
  causes 400s, drop it per route with `drop_fields` / `drop_tool_fields`.

## Development

Requires Go 1.27.

```bash
go test -race ./...
SYNTHETIC_API_KEY=... NANOGPT_API_KEY=... go run . -config config.example.json
```

## Releases

CI runs only on semver tags (`vX.Y.Z`, or `vX.Y.Z-rc.N` for pre-releases): it tests, builds and pushes
the multi-arch image to GHCR with SBOM and provenance, then creates the GitHub release with the image
digest.

```bash
git tag -a v0.2.0 -m v0.2.0 && git push origin v0.2.0
```

Image tags per release: `X.Y.Z`, `X.Y`, `X` (from 1.0.0 on) and `latest` for non-pre-releases.

## License

[MIT](LICENSE)
