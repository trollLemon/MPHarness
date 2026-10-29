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
  max_output_tokens: 2048
  context_window: 0   # 0 = auto-tune
```

| Key | Default (zero value) | Example | Effect |
|-----|----------------------|---------|--------|
| `temperature` | `0.0` | `0.2` | Randomness of sampling. Lower is more deterministic. |
| `top_p` | `0.0` | `0.95` | Nucleus sampling threshold (`0` = disabled). |
| `top_k` | `0` | `40` | Only sample from top-k tokens. `0` means greedy (`1`) when `temperature` is `0`, and is left unset when `temperature` samples, so a non-zero `temperature` is never silently overridden. |
| `max_output_tokens` | `2048` | `4096` | Caps one model turn. Prevents a runaway completion from decoding until it exhausts the context window. |
| `context_window` | `0` (auto-tune) | `8192` | Max context tokens. `0` = auto-tune from model metadata. |

### Matching the settings to your goal

- **Fixing a bug:** keep `temperature` low (`0.0` to `0.3`) so the agent behaves consistently.
- **Exploration:** a higher `temperature` makes output more varied, but also less reliable at following steps.
- **Debugging:** for developing `mph`, setting temperature to 0.0 makes the model output **the same content each** time. 

### If the model forgets to call tools

A model that finishes without calling `task_status` gets one automatic nudge to call it. There is no setting that forces tool calls; if a model ignores both the tools and the nudge, use a model that handles tool calling more reliably or make the prompt more explicit about the steps to run.

## Agent loop timeouts

The `agent` section bounds how long each step is allowed:

```yaml
agent:
  max_iterations: 10
  chat_timeout: 30m
  total_timeout: 30m
  max_output_bytes: 0   # auto-capture inline cutoff; 0 = derive from context window
  send_reasoning: true  # replay reasoning_content into later turns
```

| Key | Default | Effect |
|-----|---------|--------|
| `max_iterations` | `10` | Maximum agent loop turns. |
| `chat_timeout` | `300s` | Per-inference-call timeout. It's recommended to set this higher if running on a CPU. |
| `total_timeout` | `30m` | Overall run timeout for the whole task. |
| `max_output_bytes` | `0` | Auto-capture inline cutoff (`0` = derive from context window). |
| `send_reasoning` | `true` | Replay model `reasoning_content` into later turns. |

Timeout values accept **Go duration strings** (`"60s"`, `"10m"`, `"1h"`, `"300s"`) or raw integers (interpreted as seconds). For example, `chat_timeout: 600` = 600 seconds, `chat_timeout: "10m"` = 10 minutes.

## Captured command output

```yaml
output:
  enabled: true
  mode: auto               # auto = model opts in per call; always = every capture
  inline_max_size: 0       # 0 = use the derived tool-result budget
  max_command_size: 64MiB
  max_total_size: 512MiB
  search_max_matches: 200
```

| Key | Default | Effect |
|-----|---------|--------|
| `enabled` | `true` | `false` restores exactly today's behaviour. |
| `mode` | `auto` | `always` captures every command to a file. In `auto` the model sees uncaptured output inline until it exceeds the per-result context budget; that result is then rejected with `FAILED`, so the model retries with `capture: true` or a narrower command. |
| `inline_max_size` | `0` | Inline cutoff; `0` = derived tool-result budget. |
| `max_command_size` | `64MiB` | Per-capture ceiling. |
| `max_total_size` | `512MiB` | Per-run cumulative ceiling. |
| `search_max_matches` | `200` | Cap on line numbers returned by `output_search`; `0` = unlimited. |

A capture is written to a single raw file. `output_search` greps that file directly, so every returned match is an absolute line number and nothing is hidden behind a chunk boundary. A search with an empty `matches` list means the capture has no such line.

Sizes accept a unit string such as `64MiB`, `512MiB`, or `1GiB`, a bare byte count such as `1024` (integer or quoted), or the empty string as zero. Unit strings need no quotes because YAML already reads them as strings.

## Conversation compaction

```yaml
compaction:
  enabled: true
  threshold: 0.8           # fraction of the context window; validated to (0.5, 0.95]
  max_summary_tokens: 2048
  max_attempts: 3
```

Compaction runs when token usage reaches `threshold` of the context window. It retries an empty or too-short handover once with a corrective prompt. If it still fails, the conversation is left intact and the reason is logged on the `context_compaction` event.

## Related

- [Good models](../reference/good_models.md)
- [How tool calling works](../explaination/tool_calling.md)
- [Software architecture](../reference/arch.md)
