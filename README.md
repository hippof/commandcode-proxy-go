# commandcode-proxy (Go)

[![CI](https://github.com/liwei/commandcode-proxy-go/actions/workflows/ci.yml/badge.svg)](https://github.com/liwei/commandcode-proxy-go/actions/workflows/ci.yml)

A single-binary, dependency-free proxy for the [Command Code](https://commandcode.ai)
API. It exposes an OpenAI **Chat Completions** surface (`/v1/chat/completions`,
`/v1/models`) **and** an Anthropic **Messages** surface (`/v1/messages`), and
translates every call to Command Code's `/alpha/generate` endpoint — so any
OpenAI- or Anthropic-compatible client (the `openai`/`anthropic` SDKs, Claude
Code, Hermes Agent, `curl`) can drive a Command Code subscription unchanged.

This is a Go port of — and the actively-maintained successor to — the Python
[`commandcode-proxy`](https://github.com/liwei/commandcode-proxy) (now
deprecated), built for **one-file deployment**: a static binary you copy and run,
no runtime to install.

> Unofficial and community-maintained. Not affiliated with Command Code. It
> forwards requests to the public Command Code API using **your own** key.

## Why a proxy

Command Code's OpenAI-compatible surface (`/provider/v1/chat/completions`)
requires the paid **Provider** tier; a normal subscription only works against the
custom `/alpha/generate` endpoint. This proxy speaks `/alpha/generate` upstream
and OpenAI/Anthropic downstream, so your existing plan works from any compatible
tool.

## What it does

- `POST /v1/chat/completions` — streaming and non-streaming, with tool calling,
  reasoning (`reasoning_content`) passthrough, and image input (`image_url`
  parts; data or https URLs).
- `POST /v1/messages` — Anthropic Messages-compatible (Claude Code, the
  Anthropic SDK), including `image` blocks. Vision depends on the upstream
  model (e.g. Qwen 3.7 reads images; the deepseek v4 family ignores them).
- `GET /v1/models` — proxies Command Code's live model catalog.
- `GET /health`, plus a minimal status + request-log dashboard at `GET /admin`.

Keyless by design: every request carries the caller's own Command Code key
(`Authorization: Bearer <key>` or Anthropic's `x-api-key`), relayed verbatim.
Nothing is stored, so one instance can serve callers on different accounts.

## Build & run

Requires Go 1.23+ to build; the result needs **nothing** at runtime.

```sh
make build                      # -> ./commandcode-proxy (static, CGO disabled)
./commandcode-proxy             # serves http://127.0.0.1:8787
```

Or straight from source:

```sh
go run ./cmd/commandcode-proxy
```

For Docker, a systemd unit, client/SDK setup, and the full options reference, see
**[docs/INSTALL.md](docs/INSTALL.md)**.

Bind elsewhere with `COMMANDCODE_PROXY_HOST` / `COMMANDCODE_PROXY_PORT`
(e.g. `COMMANDCODE_PROXY_HOST=0.0.0.0` to reach it from the LAN).

## Deploy the single binary

```sh
make release                    # cross-compiled static binaries in ./dist
#   dist/commandcode-proxy-linux-amd64
#   dist/commandcode-proxy-linux-arm64
#   dist/commandcode-proxy-darwin-amd64
#   dist/commandcode-proxy-darwin-arm64
```

Copy the right one to the target host and run it — no Python, no venv, no
dependencies. Pair it with a systemd unit or a container as you like.

## Configuration

All optional, via environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `COMMANDCODE_PROXY_HOST` / `_PORT` | `127.0.0.1` / `8787` | Listen address |
| `COMMANDCODE_PROXY_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error`; each request is logged at `info`+, `warn`/`error` quiet it |
| `COMMANDCODE_MODEL_ALIASES` | – | JSON map of model-id overrides |
| `COMMANDCODE_API_BASE` | `https://api.commandcode.ai` | Upstream base URL |
| `COMMANDCODE_MAX_RETRIES` | `2` | Retries for transient upstream 5xx/429 before any content |
| `COMMANDCODE_TIMEOUT` | `300` | Upstream response-header timeout (seconds) |
| `COMMANDCODE_WORKING_DIR` | proxy's cwd | Working directory reported to Command Code as grounding context |

**Model aliases.** A request's `model` is resolved before forwarding: an exact
full-id key in `COMMANDCODE_MODEL_ALIASES` wins, then a Claude-family match — a key
of `opus`/`sonnet`/`haiku` overrides that whole family, else the built-in default
(opus → `deepseek/deepseek-v4-pro`, sonnet/haiku → `deepseek/deepseek-v4-flash`) —
then the id unchanged. So `{"opus":"zai-org/GLM-5.2"}` remaps every opus id, and
Claude Code works with no config at all.

## Connect a client

```sh
# OpenAI SDK / curl
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer user_..." -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3.7-Plus","messages":[{"role":"user","content":"Say PONG"}]}'

# Claude Code (Anthropic surface)
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_API_KEY=user_...
```

## Dashboard

A minimal status page lives at **`http://127.0.0.1:8787/admin`** — open it in a
browser for uptime, total requests, and a live table of recent requests (method,
path, model, status, latency), refreshing every few seconds.

It records request **metadata only** — never your API key or message content —
in a small in-memory ring (the last 200 requests, cleared on restart). The page
is unauthenticated and meant for loopback; don't expose it on a public bind.

## Tests

```sh
make test        # unit + route tests (fake Command Code backend, no network)
```

## Layout

| Path | Responsibility |
|---|---|
| `cmd/commandcode-proxy` | Entrypoint (env-driven host/port) |
| `internal/config` | Environment settings, model-alias resolution |
| `internal/auth` | Keyless API-key extraction |
| `internal/translate` | Pure OpenAI/Anthropic ⇄ Command Code translation |
| `internal/upstream` | `/alpha/generate` streaming, retry, error classification |
| `internal/server` | Routes, response assembly, error mapping, `/admin` dashboard |

See [docs/INSTALL.md](docs/INSTALL.md) for install/deploy/client setup,
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design rationale
(decisions **D1–D16**), [docs/ROADMAP.md](docs/ROADMAP.md) for deferred work, and
[AGENTS.md](AGENTS.md) for contributor/agent guidance.

## Credits

Translation logic ported from the Python
[`commandcode-proxy`](https://github.com/liwei/commandcode-proxy), itself ported
from [`patlux/pi-commandcode-provider`](https://github.com/patlux/pi-commandcode-provider).
Licensed under MIT — see [LICENSE](LICENSE).
