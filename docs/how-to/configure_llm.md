# Configuring the LLM

Two parts of the config control the LLM: the `model` key picks which LLM to run, and the `llm` section tunes how it samples.

## Picking a model

```yaml
model: unsloth/Qwen3-0.6B-Q8_0
```

`model` is a **downloadable id** from huggingface, not a local file path. On first use, `mph` downloads the model through the kronk/llama.cpp tooling and caches it for later runs. See [Good models](../reference/good_models.md) for guidance on choosing one.

## LLM sampling settings

```yaml
llm:
  temperature: 0.2
  top_p: 0.95
  top_k: 40
  tool_choice: auto   # auto | required | none
  max_output_tokens: 2048
  context_window: 0   # 0 = auto-tune
```

| Key | Default (zero value) | Example | Effect |
|-----|----------------------|---------|--------|
| `temperature` | `0.0` | `0.2` | Randomness of sampling. Lower is more deterministic. |
| `top_p` | `0.0` | `0.95` | Nucleus sampling threshold (`0` = disabled). |
| `top_k` | `0` | `40` | Only sample from top-k tokens. `0` means greedy (`1`) when `temperature` is `0`, and is left unset when `temperature` samples — so a non-zero `temperature` is never silently overridden. |
| `tool_choice` | `auto` | `auto` | `auto` lets model decide; `required` forces tool call; `none` disables. |
| `max_output_tokens` | `2048` | `4096` | Caps one model turn. Prevents a runaway completion from decoding until it exhausts the context window. |
| `context_window` | `0` (auto-tune) | `8192` | Max context tokens. `0` = auto-tune from model metadata. |

### Matching the settings to your goal

- **Fixing a bug:** keep `temperature` low (`0.0`–`0.3`) so the agent behaves consistently.
- **Exploration:** a higher `temperature` makes output more varied, but also less reliable at following steps.
- **Debugging:** for developing `mph`, setting temperature to 0.0 makes the model output **the same content each** time. 

### If the model forgets to call tools

If `tool_choice` is left as `auto` and the model constantly answers without acting, forcing it can help:

```yaml
llm:
  tool_choice: required
```

With `required` the model must emit a tool call on every turn, so it cannot finish without a summary-free tool call. Pair this with a prompt that tells it to finish with a plain summary message.

This can be helpful if you are using a smaller model that doesn't do well with tool calls.

## Agent loop timeouts

The `agent` section bounds how long each step is allowed:

```yaml
agent:
  max_iterations: 10
  chat_timeout: 30m
  total_timeout: 30m
```

| Key | Default | Effect |
|-----|---------|--------|
| `max_iterations` | `10` | Maximum agent loop turns. |
| `chat_timeout` | `300s` | Per-inference-call timeout. It's recommended to set this higher if running on a CPU. |
| `total_timeout` | `30m` | Overall run timeout for the whole task. |

Timeout values accept **Go duration strings** (`"60s"`, `"10m"`, `"1h"`, `"300s"`) or raw integers (interpreted as seconds). For example, `chat_timeout: 600` = 600 seconds, `chat_timeout: "10m"` = 10 minutes.

## Related

- [Good models](../reference/good_models.md)
- [How tool calling works](../explaination/tool_calling.md)
- [Software architecture](../reference/arch.md)
