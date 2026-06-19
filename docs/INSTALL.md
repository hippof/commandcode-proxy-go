# Install & usage

Operational guide for `commandcode-proxy` (Go). See the [README](../README.md)
for what it is and why, and [ARCHITECTURE.md](ARCHITECTURE.md) for the design
rationale.

- [Build & install](#build--install)
- [Run](#run)
- [Run with Docker](#run-with-docker)
- [Dashboard](#dashboard)
- [Run as a service (systemd user unit)](#run-as-a-service-systemd-user-unit)
- [Authentication](#authentication)
- [Configuration](#configuration)
- [Model aliases](#model-aliases)
- [Connect Hermes Agent](#connect-hermes-agent)
- [Connect the OpenAI SDK](#connect-the-openai-sdk)
- [Connect Claude Code](#connect-claude-code)
- [Tests](#tests)

## Build & install

Requires Go 1.23+ to build; the result needs **nothing** at runtime.

```sh
make build                      # -> ./commandcode-proxy (static, CGO disabled)
```

`make release` cross-compiles static binaries for linux/darwin × amd64/arm64 into
`./dist`. To install for your user, copy the binary onto your `PATH`:

```sh
install -Dm755 commandcode-proxy ~/.local/bin/commandcode-proxy
```

**Run from source** without building a binary:

```sh
go run ./cmd/commandcode-proxy
```

## Run

```sh
./commandcode-proxy                            # serves http://127.0.0.1:8787
```

The proxy is keyless — it needs no key to start. Every request must carry the
caller's Command Code key in `Authorization: Bearer <key>` (or Anthropic's
`x-api-key`), relayed to Command Code as-is.

Host/port: `COMMANDCODE_PROXY_HOST`, `COMMANDCODE_PROXY_PORT` (set
`COMMANDCODE_PROXY_HOST=0.0.0.0` to reach it from the LAN). See
[`.env.example`](../.env.example) for all settings.

## Run with Docker

```sh
docker compose up -d            # builds the image, serves on 127.0.0.1:8787
# or, without compose:
docker build -t commandcode-proxy .
docker run --rm -p 127.0.0.1:8787:8787 commandcode-proxy
```

The build is multi-stage: a static binary compiled in a Go image, then shipped
alone in a `scratch` image. The container is keyless like the rest of the proxy —
callers pass their key in `Authorization`. It binds `0.0.0.0` inside the
container; the published port is loopback-only on the host.

## Dashboard

A minimal status page lives at **`http://127.0.0.1:8787/admin`** — open it in a
browser for uptime, total requests, and a live table of recent requests (method,
path, model, status, latency), refreshing every few seconds.

It records request **metadata only** — never your API key or message content —
in a small in-memory ring (the last 200 requests, cleared on restart). The page
is unauthenticated and meant for loopback, matching the rest of the proxy's
security model; don't expose it on a public bind.

## Run as a service (systemd user unit)

Run the proxy as a per-user background service that survives logout and starts on
boot — no root needed.

```sh
# 1. Put the binary where the unit expects it (independent of any source checkout)
install -Dm755 commandcode-proxy ~/.local/bin/commandcode-proxy

# 2. Install + enable the user service (no key needed — callers pass their own)
mkdir -p ~/.config/systemd/user
cp deploy/commandcode-proxy.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now commandcode-proxy.service

# 3. Keep it running across reboots without an active login session
sudo loginctl enable-linger "$USER"
```

Verify and manage:

```sh
systemctl --user status commandcode-proxy.service
journalctl --user -u commandcode-proxy.service -f     # live logs
curl -s http://127.0.0.1:8787/health                  # {"status":"ok","version":"..."}
systemctl --user restart commandcode-proxy.service
systemctl --user disable --now commandcode-proxy.service   # stop + disable
```

The unit ([`deploy/commandcode-proxy.service`](../deploy/commandcode-proxy.service))
runs `~/.local/bin/commandcode-proxy`. If your binary lives elsewhere, override
`ExecStart` with a drop-in instead of editing the shipped unit:

```sh
systemctl --user edit commandcode-proxy.service
#   [Service]
#   ExecStart=
#   ExecStart=/abs/path/to/commandcode-proxy
```

The service holds no Command Code key — each caller supplies its own in the
`Authorization` header, so one instance can serve different accounts.

## Authentication

The proxy is **keyless**: it stores no Command Code key and reads none from the
environment or disk. Every request must carry the caller's key in the
`Authorization: Bearer <key>` header (or Anthropic's `x-api-key`), relayed to
Command Code unchanged. A request without a usable key gets `401`.

Because the key travels with each request, a single proxy can serve callers on
different accounts — no shared secret to centralize.

## Configuration

All settings are environment variables (see [`.env.example`](../.env.example)):

| Variable | Default | Purpose |
|---|---|---|
| `COMMANDCODE_PROXY_HOST` / `_PORT` | `127.0.0.1` / `8787` | Listen address |
| `COMMANDCODE_PROXY_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error`; `debug` logs each request |
| `COMMANDCODE_MODEL_ALIASES` | – | JSON map of model-id overrides |
| `COMMANDCODE_API_BASE` | `https://api.commandcode.ai` | Upstream base URL |
| `COMMANDCODE_MAX_RETRIES` | `2` | Retries for transient upstream 5xx/429 before any content |
| `COMMANDCODE_TIMEOUT` | `300` | Upstream response-header timeout (seconds) |

(`COMMANDCODE_MODELS_URL`, `COMMANDCODE_CLI_VERSION`, `COMMANDCODE_TASTE_LEARNING`,
`COMMANDCODE_DEFAULT_TEMPERATURE`, `COMMANDCODE_DEFAULT_MAX_TOKENS`,
`COMMANDCODE_MAX_TOKENS_CAP` are also honored — defaults shown in `.env.example`.)

## Model aliases

The proxy rewrites a request's `model` to a Command Code id before forwarding, on
**both** `/v1/chat/completions` and `/v1/messages`. Resolution order:

1. **Exact override** — `COMMANDCODE_MODEL_ALIASES`, a JSON object mapping an id
   (or short name) to a Command Code id:
   ```sh
   export COMMANDCODE_MODEL_ALIASES='{"sonnet":"anthropic/claude-...","qwen":"Qwen/Qwen3.7-Plus"}'
   ```
2. **Built-in family default** — any **opus** id → `deepseek/deepseek-v4-pro`,
   any **sonnet** or **haiku** id → `deepseek/deepseek-v4-flash`, so Claude Code's
   models work with no config. (Matched as a substring, so dated variants like
   `claude-opus-4-1-20250805` are covered.)
3. **Pass-through** — anything else is sent unchanged.

`/v1/models` still lists Command Code's full catalog.

## Connect Hermes Agent

Register the proxy as a custom provider in Hermes — two ways:

**Interactive:** run `hermes model`, choose the **Custom endpoint** provider, and
enter the base URL `http://127.0.0.1:8787/v1` plus your Command Code key.

**Or edit** `~/.hermes/config.yaml` directly:

```yaml
custom_providers:
- name: commandcode
  base_url: http://127.0.0.1:8787/v1
  api_key: user_...                 # your Command Code key (passed through to CC)
  model: deepseek/deepseek-v4-pro   # default model — any id from GET /v1/models
```

Either way, pick a `commandcode` model in `hermes model` (Hermes discovers them
from `/v1/models`). Any model your Command Code plan includes works — others
return `MODEL_NOT_IN_PLAN`.

## Connect the OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8787/v1", api_key="user_...")
resp = client.chat.completions.create(
    model="Qwen/Qwen3.7-Plus",
    messages=[{"role": "user", "content": "Say PONG"}],
)
print(resp.choices[0].message.content)
```

Or `curl`:

```sh
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer user_..." -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3.7-Plus","messages":[{"role":"user","content":"Say PONG"}]}'
```

## Connect Claude Code

The proxy also serves `POST /v1/messages` (Anthropic Messages-compatible), so
Anthropic-native clients work too. Point Claude Code at it and send your Command
Code key — the proxy accepts both `x-api-key` (Anthropic style) and
`Authorization: Bearer`:

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_API_KEY=user_...        # your Command Code key (sent as x-api-key)
```

**Model ids.** Claude Code requests Anthropic ids (`claude-opus-4-8`,
`claude-sonnet-4-6`, and the small/fast `claude-haiku-…` for background tasks)
that Command Code doesn't know. The proxy maps them by default — **opus** →
`deepseek/deepseek-v4-pro`, **sonnet** and **haiku** → `deepseek/deepseek-v4-flash`
([Model aliases](#model-aliases)) — so Claude Code works with no extra config.
Set `COMMANDCODE_MODEL_ALIASES` only to override a family or pin a specific id.

The Anthropic SDK works the same way:

```python
from anthropic import Anthropic

client = Anthropic(base_url="http://127.0.0.1:8787", api_key="user_...")
msg = client.messages.create(
    model="claude-opus-4-8",              # mapped via COMMANDCODE_MODEL_ALIASES
    max_tokens=128,
    messages=[{"role": "user", "content": "Say PONG"}],
)
print(msg.content[0].text)
```

## Tests

```sh
make test                               # unit + route tests (no network)

# opt-in live smoke against the real API (models + non-streaming + streaming):
RUN_LIVE=1 COMMANDCODE_API_KEY=user_... go test ./internal/server/ -run TestLive -v
```

Live tests hit the real API; plan-gated models (`402` / `MODEL_NOT_IN_PLAN`)
count as a pass, since they still prove the request was translated and reached
Command Code. Override the model with `COMMANDCODE_SMOKE_MODEL`.
