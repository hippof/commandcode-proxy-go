# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
- **`COMMANDCODE_PROXY_LOG_LEVEL`** (`debug`/`info`/`warn`/`error`; `debug` logs
  each request); the proxy version is surfaced at `GET /health`.
- **Single-binary distribution** — `make build` (static, CGO disabled),
  `make release` (cross-compiles linux/darwin × amd64/arm64), a `Dockerfile` +
  `compose.yaml`, and a systemd user unit in `deploy/`.

[1.0.0]: https://github.com/liwei/commandcode-proxy-go/releases/tag/v1.0.0
