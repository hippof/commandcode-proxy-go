# Architecture

`commandcode-proxy` (Go) is a small, stateless HTTP service that exposes an
**OpenAI Chat Completions**– and **Anthropic Messages**–compatible API and
forwards each request to the **Command Code** API, translating both the request
and the streamed response. It lets any OpenAI- or Anthropic-compatible tool drive
a Command Code subscription without that tool knowing anything about Command Code.

It is a stdlib-only Go port of the Python
[`commandcode-proxy`](https://github.com/liwei/commandcode-proxy), built for
single-binary deployment. This document explains *why* the system is shaped the
way it is. The numbered decisions in [§4](#4-design-decisions) are the important
part; the rest is context for them.

---

## 1. The problem

Command Code exposes **two** inference surfaces, and they are not interchangeable:

| Surface | Protocol | Who can use it |
|---|---|---|
| `POST /provider/v1/chat/completions` | OpenAI-compatible | Requires the paid **Provider** tier |
| `POST /alpha/generate` | Custom (Vercel-AI-SDK v5 event stream) | Covered by a normal **subscription** (Go / Pro / …) |

Pointing an OpenAI client at `/provider/v1` fails for anyone not on the Provider
tier (`403 MODEL_NOT_IN_PLAN` / `upgrade_required`). The endpoint a normal
subscription *does* cover (`/alpha/generate`) speaks a bespoke protocol no
off-the-shelf client understands.

**Goal:** make the `/alpha/generate` surface usable from *any* OpenAI- or
Anthropic-compatible tool, with no per-tool code — as one static binary.

---

## 2. Solution shape

A standalone translating proxy:

```
┌─────────────┐  OpenAI / Anthropic        ┌────────────────────┐  Command Code        ┌──────────────┐
│  Consumers  │  POST /v1/chat/completions │  commandcode-proxy │  POST /alpha/generate│  Command Code│
│  • openai   │ ─────────────────────────▶ │  (net/http)        │ ───────────────────▶ │  API         │
│  • Claude   │                            │                    │                      │  (your plan) │
│    Code     │  SSE chunks / JSON         │  translate ⇄       │  NDJSON event stream │              │
│  • Hermes   │ ◀───────────────────────── │      upstream      │ ◀─────────────────── │              │
└─────────────┘                            └────────────────────┘                      └──────────────┘
```

### Package responsibilities

| Package | Responsibility |
|---|---|
| `cmd/commandcode-proxy` | Entrypoint: env-driven host/port, starts `http.Server` |
| `internal/server` | Routes, response assembly, error mapping, the shared HTTP client, `/admin` dashboard + request-log middleware |
| `internal/translate` | **Pure** OpenAI/Anthropic ⇄ Command Code translation (request build, event→chunk/completion/message) |
| `internal/upstream` | The single upstream call: stream `/alpha/generate`, parse NDJSON, retry-before-content, classify errors |
| `internal/auth` | Extract the caller's Command Code key from `Authorization` / `x-api-key` |
| `internal/config` | Environment-driven settings, model-alias resolution |

Dependency direction is one-way: `server → {auth, config, translate, upstream}`,
`upstream → {translate, config}`. `translate`, `auth`, and `config` import
nothing from the rest of the module, which keeps them unit-testable without a
server or network. The Anthropic translation lives in `translate` too
(`anthropic.go`) so it can reuse the unexported Command Code helpers directly,
the same way the Python `translate_anthropic` reaches into `translate`.

---

## 3. Request lifecycle

A `POST /v1/chat/completions` is handled in one pass (`server.chatCompletions`):

```
1.  parse JSON body                                          (server)
2.  read API key from Authorization: Bearer <key>            (auth)        → 401 if none
3.  resolve the model id (alias → family default → as-is)    (config)
4.  build the Command Code body                              (translate.BuildCCRequest)
5.  open the upstream stream to /alpha/generate              (upstream.NewStream)
6.  advance to the first DECISIVE event                      (upstream.AdvanceToDecisive)
        • non-200 HTTP (non-retryable) → *UpstreamError → error JSON (status passthrough)
        • first event is `error`       → error JSON (status from envelope)
        • stream ended empty           → empty completion
7.  with that first event, either:
        • stream:  emit OpenAI SSE chunks                    (translate.StreamCompletion)
        • buffer:  accumulate one chat.completion            (translate.BuildCompletion)
```

`/v1/messages` is identical except it adapts the Anthropic body first
([D14](#d14-anthropic-messages-by-reusing-the-openai-pipeline)) and reshapes the
response into Anthropic events. Step 6 is the load-bearing one — see
[D5](#d5-peek-the-first-decisive-event-before-committing-a-response).

---

## 4. Design decisions

Each is **Context → Decision → Why → Trade-offs**. The decision numbers match the
Python project's `docs/ARCHITECTURE.md` so cross-references stay aligned.

### D1. A proxy, not an agent plugin

**Decision.** A standalone OpenAI/Anthropic-compatible proxy, not a plugin wired
into one agent. **Why.** The same running binary serves any compatible client
unchanged, with zero coupling — nothing downstream has to know Command Code
exists. **Trade-offs.** A separate process to run; trivially cheap here (one
static binary).

### D2. OpenAI Chat Completions + Anthropic Messages as the public contracts

**Decision.** Speak the two de-facto lingua francas. **Why.** Together they cover
nearly every agent/SDK with zero adaptation. **Trade-offs.** Neither schema
expresses everything (e.g. signed thinking blocks); lossy mappings are accepted
([D13](#d13-faithful-port--deliberate-non-goals)).

### D3. Target `/alpha/generate`, not `/provider/v1`

**Decision.** Forward to the custom surface a normal subscription covers. **Why.**
`/provider/v1` needs the paid Provider tier; using it would defeat the proxy's
purpose. **Trade-offs.** We own a translation layer; `COMMANDCODE_API_BASE` is
configurable so a Provider-tier user could re-point if they wanted.

### D4. Translation is a layer of pure functions

**Decision.** All translation lives in `internal/translate` as pure functions —
no network, no file/env I/O, no global state. `upstream` does I/O; `server` does
HTTP. **Why.** Purity makes the risky logic exhaustively unit-testable with plain
maps and a `next()` event source — no server, no live key. **Trade-offs.** A
little ceremony (threading `cid`/`created`/`model` through), worth the test
surface.

### D5. Peek the first decisive event before committing a response

**Context.** Command Code reports failures two ways: non-200 HTTP (e.g. `403` for
a plan problem) **and** `200 OK` followed by an in-stream `error` event. Once a
streamed `200` is sent, the status can't change.

**Decision.** Before writing any response, drain leading non-decisive events and
inspect the first decisive one (`upstream.AdvanceToDecisive` over the pull-based
`Stream`). A non-200 HTTP open or an `error`-first stream becomes a proper error
**JSON with the real status**. Only a real content/finish first event commits to
a `200`, with that first event handed to the translator.

**Why.** Clients get correct HTTP semantics — a plan error is a real `403`, not a
`200` stream hiding an error. The single most important correctness decision.
**Trade-offs.** First-byte latency includes the upstream's first decisive event
(in practice the first token — negligible).

### D6. Retry only *before* the first content event

**Decision.** `upstream.Stream` retries (bounded, exponential backoff) only when a
transient failure arrives and **no content event** (`text-delta` /
`reasoning-delta` / `tool-call`) has been emitted yet. Two transient cases are
retried: a retryable **HTTP status** (`429`/`5xx`) at open, and an in-stream
`error` event flagged `isRetryable`.

**Why.** Retrying after visible output would duplicate/corrupt the response; a
clean pre-content retry is invisible to the client and smooths over upstream
blips. **Trade-offs.** A mid-stream failure is surfaced, not hidden (correct, but
a partial turn). Bounded by `COMMANDCODE_MAX_RETRIES` (default 2). Since a non-200
arrives before any event, the HTTP-status retry never violates the
"no retry after content" rule.

### D7. Keyless pass-through

**Decision.** The proxy holds no key. `auth.ResolveAPIKey` reads the caller's key
from `Authorization: Bearer` or `x-api-key` (ignoring known placeholders) and
relays it unchanged; no key → `401`. **Why.** A stateless relay is simpler and
safer — each caller brings its own key, so one instance serves different accounts
with no shared secret to provision or leak. As a backstop, upstream-derived error
text is scrubbed of credential-shaped substrings (`translate.RedactSecrets`,
ported from the pi extension) before it reaches a client, in case Command Code
ever echoes a key back. **Trade-offs.** No "centralize the secret" mode; for the
common single-user case that's just one config line.

### D8. Always stream upstream; buffer downstream on demand

**Decision.** Always request `stream: true` upstream; downstream branch on the
client's `stream` flag — `translate.StreamCompletion`/`StreamMessage` emit SSE, or
`translate.BuildCompletion`/`BuildMessage` accumulate one response. **Why.** One
upstream path feeds both client modes, so streamed and buffered outputs are
guaranteed consistent (same events through shared helpers). **Trade-offs.**
Non-streaming clients wait for the full upstream stream — inherent to a
stream-only upstream.

### D9. Stateless requests

**Decision.** Every request is independent: a fresh `threadId` per call and a
neutral, minimal `config` block. No session store. **Why.** Chat Completions is
itself stateless (full history arrives each turn), so there's nothing to remember;
statelessness keeps the proxy trivially scalable. **Trade-offs.** No per-thread
reuse; costs nothing observable (CC prompt caching keys on content prefix, not
`threadId`).

### D10. One shared HTTP client, streaming-friendly

**Context.** Per-request clients waste connections; a total client timeout would
cut long streams.

**Decision.** A single `*http.Client` built once in `server.New`, with a custom
`Transport` that bounds the connect and response-header waits but sets **no total
timeout**, so long SSE streams aren't truncated. `http.Client` is
concurrency-safe and long-lived, so no per-request lifecycle is needed.

**Why.** Connection pooling and one place to configure timeouts, without the
lifespan/singleton ceremony the Python version needs. **Trade-offs.** A hung
upstream relies on the response-header timeout + request context for cancellation,
not a blanket deadline — the right trade for a streaming proxy.

### D11. Error model: translate envelopes, pass the status through

**Decision.** Normalize Command Code errors (HTTP body
`{"success":false,"error":{code,status,message}}` or stream
`{"type":"error","error":{message,code,statusCode,isRetryable}}`) into the
client's envelope — OpenAI `{"error":{message,type,code}}` or Anthropic
`{"type":"error","error":{type,message}}` — and pass the upstream **status
through** (`upstream.extractError`, `server.statusFromCCError`), defaulting to
`502`. **Why.** Clients get a clean, correctly-shaped error *and* a meaningful
status (a plan problem is a real `403`, a bad key a real `401`).

### D12. Configuration via environment (12-factor)

**Decision.** Everything tunable is an env var with a sensible default in
`internal/config` (`COMMANDCODE_API_BASE`, `…_MODEL_ALIASES`, `…_MAX_RETRIES`,
`…_TIMEOUT`, sampling defaults, host/port). **Why.** No config files; trivial to
run in a shell, a container, or a service manager.

### D13. Faithful port — deliberate non-goals

Ported the translation and auth; deliberately **left out**: the browser `/login`
OAuth flow (each caller passes its own key) and a static pricing table (token
`usage` passes through verbatim). The boundary is intentional, not an oversight.
Image inputs were originally left out too, but a live probe (2026-07-02)
confirmed upstream support, so they are now forwarded — see D14 and
[ROADMAP.md](ROADMAP.md) for what remains deferred.

### D14. Anthropic Messages by reusing the OpenAI pipeline

**Decision.** `POST /v1/messages` adapts the Anthropic body into the OpenAI
request shape (`translate.OpenAIRequestFromAnthropic`), then runs the **same**
`BuildCCRequest` → `upstream` → translate path; the response is reshaped into
Anthropic (buffered via `BuildCompletion` → `MessageFromCompletion`, streaming via
a `content_block_*` state machine).

**Alternative considered: a direct Anthropic → Command Code translator.** Rejected
— it would put Command Code's finicky wire format (system-lift, typed parts,
`input_schema` tools, and especially the **dangling tool-call pruning** CC rejects
requests without) in *two* encoders. Normalizing to the OpenAI shape lands exactly
where the CC encoder already operates; Anthropic (everything-is-a-block) is the
structural outlier, so the OpenAI hop is a funnel, not a lossy U-turn. The one
feature the hop "drops" (`tool_choice`) isn't caused by the hop — CC has no
forced-tool: a live probe (2026-08) confirmed `/alpha/generate` calls the tool
when the prompt asks but ignores `tool_choice` in every form (`auto`/`any`/named),
so forwarding it is a no-op and a direct path would drop it too. An input
`thinking` budget maps
onto CC's `params.reasoning_effort` (≤4k → `low`, ≤16k → `medium`, else `high`,
mirroring Claude Code's think/megathink/ultrathink presets), the same field the
OpenAI surface fills from `reasoning_effort` (`minimal` lowers to `low`, `none`
omits); values pass through for CC to validate per model, matching how the pi
extension sends its supported effort levels.

**Images through the hop.** A live probe (2026-07-02) showed `/alpha/generate`
accepts image parts in a user message's content array — both the AI-SDK shape
`{"type":"image","image":<data:/https: URL>}` and Anthropic image blocks
verbatim. Both surfaces now emit the shape the current Command Code CLI
(`command-code@1.15.1`) sends: `{"type":"image","image":<URL>}` plus a
`mimeType` field when knowable (always for data: URLs). OpenAI `image_url`
parts map straight to it; the Anthropic path converts a `base64` source into a
data: URL (so `cache_control` and other extras don't leak) and passes `url`
sources through; text-only content keeps the historical flattened-string shape.
Images inside tool results get special handling: the probe showed CC silently
drops media parts in tool-result outputs, so — like the CLI — the proxy
re-emits tool-result images as a follow-up user turn, where they demonstrably
reach the model. Vision is per model — deepseek v4 silently ignores images,
GLM-5.2 rejects them in-stream, Qwen 3.7 reads them — and those upstream
behaviors surface to the caller unchanged, like `MODEL_NOT_IN_PLAN`.

**Thinking signatures.** Anthropic signs `thinking` blocks cryptographically and a
third-party proxy can't mint a valid signature, but Claude Code only checks the
payload's first byte is `0x12` (base64 then starts with `E`). So a returned
thinking block carries a synthetic signature — `base64(0x12 ++ len ++
sha256(thinking))` — seeded by the block's own text, on both the buffered message
(a `signature` field) and the stream (a `signature_delta` before the block's
`content_block_stop`). Without it Claude Code drops the thinking rather than
rendering it.

### D15. Observability: an in-memory request log + dashboard

**Decision.** `internal/server` keeps a bounded ring (200 entries) of request
**metadata** — timestamp, method, path, resolved model, status, latency — filled
by a logging middleware. `GET /admin` serves a self-contained HTML page (no
external assets) that polls `GET /admin/data`. No new dependencies, no
persistence, no auth.

**Why / Go mechanics.** The middleware wraps the `http.ResponseWriter` but
**delegates `http.Flusher`**, so SSE keeps flushing in real time (D8) — the Go
analog of the Python "pure-ASGI, never buffer" rule. The resolved model flows from
the handler back to the middleware through a **context-carried pointer**
(`modelCarrier`), the Go analog of ASGI scope state. `/admin*` and `/health` are
excluded to keep the log to real API traffic. **Trade-offs.** Lost on restart, not
shared across processes (fine for the loopback default); unauthenticated, so it
must stay on a trusted bind.

### D16. Build responses as `map[string]any`, not structs

**Context.** The wire shapes have many nullable/optional fields with exact
semantics: `message.content` is `null` (not `""`) when empty, `finish_reason` is
`null` in content chunks, `usage` is omitted when absent, an empty Anthropic text
block must still carry `"text": ""`.

**Decision.** Build outgoing JSON as `map[string]any` (mirroring the Python dicts)
rather than tagged structs.

**Why.** A map reproduces every null/omit/empty nuance trivially and 1:1 with the
reference implementation, which is exactly what the wire contract requires. Structs
with `omitempty`/pointers fight these cases and are easy to get subtly wrong.
**Trade-offs.** No compile-time typing on output shapes, and Go marshals map keys
alphabetically — cosmetic only, since clients parse JSON by key. Hot paths could be
converted to typed structs later if stricter typing is wanted.

---

## 5. Data translation reference

### Messages (OpenAI → Command Code)

| OpenAI | Command Code |
|---|---|
| `system` / `developer` role | lifted into `params.system` (joined) |
| `user` | `{role:"user", content:<text>}` |
| `assistant` text | `{type:"text", text}` part |
| `assistant.tool_calls[]` | `{type:"tool-call", toolCallId, toolName, input}` parts |
| `tool` role result | `{type:"tool-result", toolCallId, toolName, output:{type:"text", value}}` |

Dangling tool calls/results are pruned: only `tool_call_id`s appearing in **both**
an assistant `tool_calls` and a `tool` result survive (`pairedToolCallIDs`),
matching what Command Code accepts.

### Events (Command Code → OpenAI)

| Command Code event | OpenAI |
|---|---|
| `text-delta` | `choices[].delta.content` |
| `reasoning-delta` | `choices[].delta.reasoning_content` |
| `tool-call` | `choices[].delta.tool_calls[]` (indexed; full id+name+arguments) |
| `finish` | `finish_reason` (`tool-calls`→`tool_calls`, `length`/`max_*`→`length`, else `stop`) |
| `finish.totalUsage` | `usage` (`prompt`/`completion`/`total_tokens`, `prompt_tokens_details.cached_tokens`) |
| `error` | error frame (streaming) / error JSON (buffered) |
| `start`, `start-step`, `reasoning-start/-end` | dropped (non-decisive) |

---

## 6. Security model

- The proxy relays a Command Code key (it stores none) and should bind to
  **loopback** (`127.0.0.1`, the default). Anything that can reach the port can
  submit requests with a key it already holds.
- The key is never logged. Errors carry Command Code's message text only.
- Per-caller isolation is the default ([D7](#d7-keyless-pass-through)). The
  dashboard records metadata only — never a key, never message content.

---

## 7. Testing strategy

| Layer | File | Network |
|---|---|---|
| Pure translation (request build, dangling pruning, parsing, Anthropic fan-out) | `internal/translate/translate_test.go` | none |
| Keyless key extraction · config/alias resolution | `internal/auth`, `internal/config` | none |
| Upstream retry (HTTP 5xx/429, exhaustion, no-retry-4xx, in-stream isRetryable) | `internal/upstream/upstream_test.go` | none |
| Routes against a fake Command Code backend over real HTTP (auth, success, tool calls, streaming, alias, `/v1/messages`, `/admin`, error mapping) | `internal/server/*_test.go` | loopback only |

Route tests run a `httptest` Command Code backend, so they exercise the real
`upstream` + `translate` path end to end — stronger than mocking the event stream.

---

## 8. Module map

```
cmd/commandcode-proxy/      entrypoint (env-driven host/port)
internal/
├── config/                 environment settings, model-alias resolution
├── auth/                   API-key extraction (Authorization / x-api-key)
├── translate/             OpenAI/Anthropic ⇄ Command Code translation (pure)
│   ├── translate.go        request build, parsing, shared mappers
│   ├── openai_response.go  chat.completion / chunk / SSE builders
│   └── anthropic.go        Anthropic request adapt + Message/SSE builders
├── upstream/               /alpha/generate streaming, retry, error classification
└── server/                 routes, response assembly, error mapping, /admin dashboard
```
