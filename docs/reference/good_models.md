# Good models

`mph` runs the LLM fully local via the kronk/llama.cpp tooling, and the `model` key in your config is a **downloadable id** (downloaded and cached on first use), not a file path. This page is a reference for choosing one.

## What to look for

| Property | Importance |
|----------|----------------|
| **Tool-calling ability** | The agent's whole workflow relies on reliable function/tool calling (`multipass_*`). Choose a model that is explicitly trained for it (for example the Qwen3 generation). |
| **Quantisation** | Q8_0 weights give near precision at a fraction of the size; a good default. Smaller quantisations (Q4, Q6) squeeze onto less RAM/VRAM at some quality cost. |
| **Context window** | Enough context to hold the conversation + tool results. If you don't tune it, `mph` auto-tunes from the model metadata; set `llm.context_window` for control. |
| **Speed** | CPU-only machines can run larger models, but make sure to set a high `agent.chat_timeout`.|

## Example ids

The `unsloth/Qwen3-*` family is a reliable starting point. Multiples of size follow a clear pattern: pick the largest that fits your machine:

| Id | Approx. Q8_0 memory | Notes |
|----|---------------------|----------|
| `unsloth/Qwen3-0.6B-Q8_0` | ~0.7 GB | Fast smoke tests on CPU-only boxes. |
| `unsloth/Qwen3-1.7B-Q8_0` | ~1.9 GB | Reasonable tool-calling on modest hardware. |
| `unsloth/Qwen3-4B-Q8_0` | ~4.4 GB | Better instruction-following; fits a mid-range GPU or decent RAM. |
| `unsloth/Qwen3-8B-Q8_0` | ~8.8 GB | Strong tool use; needs a real GPU or lots of system RAM. |
| `unsloth/Qwen3-14B-Q8_0` | ~15 GB | Highest quality; requires generous VRAM/RAM. |


## Related

- [Configuring the LLM](../how-to/configure_llm.md)
- [How tool calling works](../explaination/tool_calling.md)
