# Roadmap

Deferred enhancements, captured so the boundary stays intentional. Not started;
needs design before code.

## Multimodal — remaining gaps

Image input is **implemented** — user messages on both surfaces, and images
inside tool results, which are re-emitted as a follow-up user turn because a
probe (2026-07-02) showed upstream silently drops media parts in tool-result
outputs (the same workaround the Command Code CLI ships; see
[D14](ARCHITECTURE.md#d14-anthropic-messages-by-reusing-the-openai-pipeline)
for the probe findings and mapping). Still deferred:

- **Document/PDF input.** The upstream schema's part union advertises a
  `document` block (`source`-shaped, like Anthropic's); unprobed. Candidate for
  PDF support on both surfaces later.
- **Vision capability discovery.** The model catalog carries no modality
  metadata; deepseek v4 silently ignores images, GLM-5.2 rejects them, Qwen 3.7
  reads them. Callers find out from model behavior. A curated capability map was
  considered and skipped — it would rot as the catalog changes.
- **Model-side limits.** Upstream enforces per-model image constraints (e.g.
  both dimensions > 10 px on the Qwen backend); the proxy does not pre-validate,
  it relays the upstream error.
