# Roadmap

Deferred enhancements, captured so the boundary stays intentional. Not started;
needs design before code.

## Image / multimodal input

Today image content parts are flattened to their text (`translate.contentToText`,
used by both the OpenAI and Anthropic paths), matching the text-only upstream.
Native image support would mean:

- Probe whether Command Code's `/alpha/generate` accepts image parts at all, and
  in what shape — this gates the whole feature.
- If supported, map OpenAI `image_url` / base64 parts (and Anthropic `image`
  blocks) to Command Code's part type, instead of dropping to text. Per
  [D14](ARCHITECTURE.md#d14-anthropic-messages-by-reusing-the-openai-pipeline),
  this is the point where the Anthropic→OpenAI funnel stops paying off for image
  content, so it becomes a direct part-to-part mapping on *both* surfaces.
- Decide size/format limits and the error behavior for unsupported inputs.

Until upstream support is confirmed, the flatten-to-text behavior stays.
