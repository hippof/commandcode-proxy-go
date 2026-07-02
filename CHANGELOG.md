# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Hardening for exposed binds**: a 10s `ReadHeaderTimeout` (Slowloris guard),
  a 32 MiB request-body cap (over-limit requests get a proper `413`), and
  graceful shutdown on SIGINT/SIGTERM (in-flight requests, including SSE
  streams, get up to 30s to finish). Streaming behavior is unchanged — there is
  still no read/write timeout on established requests.

## [1.2.0] - 2026-07-02

### Added
- **Image input** on both surfaces, verified against the live API. OpenAI
  `image_url` parts (data: or https: URLs) and Anthropic `image` blocks
  (`base64` and `url` sources) are forwarded to Command Code as typed content
  parts; text-only content keeps its flattened-string shape. Vision is
  per-model: Qwen 3.7 reads images, the deepseek v4 family silently ignores
  them, GLM-5.2 rejects them — upstream behavior is relayed unchanged.

## [1.1.1] - 2026-06-20

### Fixed
- **Anthropic `/v1/messages` echoes the requested model** in the response `model`
  field (and `message_start`), not the resolved upstream id. Claude Code can now
  restore a session whose model was aliased (e.g. `claude-opus-4-8` →
  `zai-org/GLM-5.2`); previously it stored the upstream id and reported it could
  not be restored. The resolved model is still sent upstream and shown in the
  access log / dashboard.

## [1.1.0] - 2026-06-20

### Added
- **Family-level model aliases** — a `COMMANDCODE_MODEL_ALIASES` key of `opus`,
  `sonnet`, or `haiku` now overrides that whole family (any id containing it), e.g.
  `{"opus":"zai-org/GLM-5.2"}`. Exact full-id keys still take precedence.

## [1.0.0] - 2026-06-19

First release of the Go port — a single static binary, dependency-free, with the
same public contracts as the Python
[`commandcode-proxy`](https://github.com/liwei/commandcode-proxy). Input is
text-only; image/multimodal parts are flattened to text (see `docs/ROADMAP.md`).

### Added
- **OpenAI Chat Completions** proxy over Command Code's `/alpha/generate`:
  `POST /v1/chat/completions` (streaming and buffered, with tool calling and
  `reasoning_content`), `GET /v1/models`, `GET /health`.
- **Anthropic Messages-compatible `/v1/messages`** — Claude Code and the Anthropic
  SDK, streaming and buffered, with thinking and `tool_use` blocks. Translated
  through the shared OpenAI pipeline (see `docs/ARCHITECTURE.md` D14).
- **Keyless** design — every request carries the caller's own Command Code key
  (`Authorization: Bearer` or Anthropic's `x-api-key`), relayed verbatim; nothing
  is stored.
- **Model aliases** — `COMMANDCODE_MODEL_ALIASES` (JSON) plus built-in family
  defaults so Claude Code works with no config: any **opus** id →
  `deepseek/deepseek-v4-pro`, any **sonnet** or **haiku** id →
  `deepseek/deepseek-v4-flash`.
- **Peek-first-decisive-event** handling so upstream errors surface as real HTTP
  statuses, with retry of transient `429`/`5xx` and `isRetryable` stream errors
  only before the first content token.
- **`/admin` dashboard** — status page with uptime and a live request log
  (metadata only — never keys or content; last 200 requests, in-memory).
- **`COMMANDCODE_PROXY_LOG_LEVEL`** (`debug`/`info`/`warn`/`error`); each request
  is logged at `info` (the default) and above — an access log of method, path,
  status, latency, and model — with `warn`/`error` quieting it. The proxy version
  is surfaced at `GET /health`.
- **Single-binary distribution** — `make build` (static, CGO disabled),
  `make release` (cross-compiles linux/darwin × amd64/arm64), a `Dockerfile` +
  `compose.yaml`, and a systemd user unit in `deploy/`.

[1.2.0]: https://github.com/liwei/commandcode-proxy-go/releases/tag/v1.2.0
[1.1.1]: https://github.com/liwei/commandcode-proxy-go/releases/tag/v1.1.1
[1.1.0]: https://github.com/liwei/commandcode-proxy-go/releases/tag/v1.1.0
[1.0.0]: https://github.com/liwei/commandcode-proxy-go/releases/tag/v1.0.0
