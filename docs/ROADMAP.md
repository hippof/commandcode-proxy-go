# Roadmap

Deferred enhancements, captured so the boundary stays intentional. Not started;
needs design before code.

## Multimodal — remaining gaps

Image input in user messages is **implemented** (OpenAI `image_url` parts and
Anthropic `image` blocks forward as typed parts; verified live — see
[D14](ARCHITECTURE.md#d14-anthropic-messages-by-reusing-the-openai-pipeline)
for the probe findings and mapping). Still deferred:

- **Document/PDF input.** The upstream schema's part union advertises a
  `document` block (`source`-shaped, like Anthropic's); unprobed. Candidate for
  PDF support on both surfaces later.
- **Images inside `tool_result` content.** Tool results still flatten to text
  (`toolResultToText`); Anthropic allows image blocks there.
- **Vision capability discovery.** The model catalog carries no modality
  metadata; deepseek v4 silently ignores images, GLM-5.2 rejects them, Qwen 3.7
  reads them. Callers find out from model behavior. A curated capability map was
  considered and skipped — it would rot as the catalog changes.
- **Model-side limits.** Upstream enforces per-model image constraints (e.g.
  both dimensions > 10 px on the Qwen backend); the proxy does not pre-validate,
  it relays the upstream error.
