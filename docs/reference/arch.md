# Software architecture

This is the reference description of how MPHarness is structured and how it works internally. For hands-on usage, see the [tutorial](../tutorial/run_an_agent_with_task.md) and [how-to guides](../how-to/) instead.

## Purpose

MPHarness is a local harness for running agent-driven technical tasks inside an [LXD](https://linuxcontainers.org/lxd/) VM. A small locally-run LLM acts as an autonomous agent that drives an LXD VM through a *tool-calling* loop to complete a task described in a YAML config. Running the agent in a disposable VM keeps any damage it does contained.

## High-level layout

```mermaid
flowchart TD
    CLI["mph (CLI entry)"]
    CONF["internal/config<br/>YAML parsing, validation, defaults"]
    OTEL["internal/otlp<br/>OTLP log pipeline: Logger provider + zerolog Hook"]
    AGENT["internal/agent<br/>Agent loop, LLM client, tool specs + dispatch"]
    HARNESS["internal/harness<br/>VM lifecycle: info, launch, prompt, delete"]
    LXD["internal/lxd<br/>LXD daemon client"]
    VALIDATE["internal/validation<br/>shell AST allowlist check (fail closed)"]

    CLI --> CONF
    CLI --> OTEL
    CLI --> HARNESS
    HARNESS --> AGENT
    AGENT --> LXD
    AGENT --> VALIDATE
    AGENT -.-> OTEL
```

Tool dispatch (`Call`, plus `callExec`, `callOutputSearch`, `callOutputRead`; `vm_info` is handled inline in `Call`) lives in `internal/agent/spec.go` alongside tool specifications.

## Package responsibilities

| Package | Responsibility |
|---------|----------------|
| `mph` | CLI entry point. Parses flags (`-i` to reuse existing VM, `-k` to keep VM after run, `--otel`/`MPH_OTEL` for OTLP logging), configures zerolog logging, loads config with `config.LoadFileWithSources`, calls `otlp.Setup`, installs the zerolog Hook, constructs the LXD client, and runs the harness with a deferred `shutdown`. |
| `internal/config` | Config model, loading, validation, and defaults. Merges YAML, supported `MPH_*`/`OTEL_*` environment variables, and explicit flags via koanf (flags > env > YAML > defaults). Owns the `allowed_commands` -> allowlist map, the `coreUtils` meta expansion, the optional `content_dir` (validated to be an existing dir, copied to `VMContentDir = "/home/ubuntu/content"`), and the optional `otel:` block (`enabled`, `endpoint`, `service_name`, `resource_attributes`); `enabled` defaults to `false`, while endpoint/service name are left empty for `mph.resolveOtelConfig` to fill in. |
| `internal/otlp` | Owns the OTLP log pipeline. `Setup(Config)` creates the `otlploggrpc` exporter, builds a `LoggerProvider` (`sdk/log`) with a `resource` (`service.name` + `OTEL_RESOURCE_ATTRIBUTES` + YAML `resource_attributes`), sets `global.SetLoggerProvider`, and returns a `shutdown` that flushes. `Hook` bridges every zerolog event to OTLP logs (level 1:1, body = message); `LogEvent` writes structured fields to both. There are no OTel traces or metrics. |
| `internal/agent` | The agent loop, the LLM client, tool specifications, the tool dispatcher, and conversation compaction. Each `Execute` iteration logs structured `event=` lines (`token_usage`, `iteration`, `agent_tool_call`, `tool_succeeded`/`tool_failed`, `tool_output_too_large`, `context_overflow`, `context_compaction`, `run_summary`) that the Grafana dashboard queries with LogQL. System prompt is built via `buildSystemPrompt(cfg)` which appends a content notice when `content_dir` is set. |
| `internal/harness` | Orchestrates VM lifecycle: checks if VM exists, launches if needed, prompts to reuse existing VM (unless `-i`), starts a stopped instance and rejects an existing instance that is not a virtual machine, copies `content_dir` to `VMContentDir` via `mkdir -p` + `Transfer` (if set), runs the agent, and deletes VM after task (unless `-k`). |
| `internal/lxd` | Concrete LXD client over the local Unix socket. Resolves `vm.image` through simplestreams remotes (`ubuntu`, `ubuntu-daily`, `images`), normalizes disk and memory sizes to binary units, launches `virtual-machine` instances with CPU/memory limits and the configured root disk size, waits for the guest to answer, and caches the `ubuntu` UID/GID and home. Implements exec (secret substitution and output scrubbing), recursive transfer, kernel install, info (`GetInstanceFull` JSON), and delete. |
| `internal/validation` | Parses a shell command into an AST (via `mvdan.cc/sh/syntax`) and checks every executed command against the allowlist; fails closed on anything it cannot verify. |
| `internal/output` | VM-side output store: capture (`<cmd> > <raw> 2>&1`), handle registry, POSIX ERE `output_search` over the raw capture file. Paths derive only from the registry. |

## Runtime flow

1. **Config load.** `config.LoadFileWithSources` merges YAML, environment, and explicit flags via koanf, then applies defaults (VM name `mph-vm`, `vm.image` `ubuntu:24.04`, `tool_choice: auto`, `max_iterations: 10`, `chat_timeout: 300s`, `total_timeout: 30m`) and validates required fields (`vm.disk`, `vm.ram`, `vm.cpu > 0`, `model`, `prompt`). `vm.name` must follow DNS hostname rules, so underscores are rejected. `content_dir` when set must be an existing local directory. OTel `enabled` defaults to `false`; `otel.endpoint`/`otel.service_name` are filled in later by `mph.resolveOtelConfig` (`localhost:4317`/`mph`).

2. **OTel setup (if enabled).** `mph` calls `otlp.Setup` with the resolved config (OTLP/gRPC log exporter + `LoggerProvider` + `resource`) and installs the zerolog `Hook` on `log.Logger` so every later `log` line is also exported.

3. **Model initialisation.** `InitializeModelFiles` detects/downloads the native llama.cpp libraries, initialises the kronk runtime, and downloads the model identified by `cfg.Model`. `NewKronk` loads the model into memory (with optional `context_window`; `0` means auto-tune).

4. **Agent construction.** `NewAgent` bundles the logger, the kronk client (see `Kronk` interface below), iteration/timeout bounds, and sampling config.

5. **VM lifecycle & agent loop.** `harness.Start`:
   - Calls `Info` to check if the named VM exists.
   - If not found: resolves the image and launches the VM with the configured spec, then waits for the guest to answer.
   - If `vm.kernel` is set: installs the kernel package and its modules, updates grub, reboots the VM, waits for it to come back online, and verifies `uname -r` matches the expected version.
   - If found and `-i` not passed: prompts the user to continue with the existing VM. A stopped instance is started and awaited. An instance that is not a virtual machine is rejected.
   - If `content_dir` is set: `mkdir -p /home/ubuntu/content` then a recursive transfer before the agent starts.
   - Runs `Agent.Execute` with the LXD client.
   - On completion, deletes the VM unless `-k` was passed.

6. **Agent loop.** `Agent.Execute(ctx, ...)` derives the total-timeout ctx from the run ctx, then per iteration logs `token_usage` and `iteration` events plus tool log events:
   - Builds the system prompt via `buildSystemPrompt(cfg)` (fixed contract + optional content notice for `/home/ubuntu/content`) and user prompt (allowed-commands line + task).
   - Builds tool documents from `Specs()` so the model knows the tools: `vm_exec` and `vm_info`, plus `output_search` and `output_read` when output captures are enabled.
   - Repeats, up to `max_iterations` times: send messages + tools to the LLM, extract the assistant message, and either
     - **terminate** when the model replies with no tool calls (its final summary), or
     - **execute** the tool calls, append their results to the conversation, and continue.
   - Independent tool calls are batched into one turn (`parallel_tool_calls: true`, and the "Batching Work" section of the system prompt); dependent ones are chained with `&&` inside a single `vm_exec`. Note that kronk parses `parallel_tool_calls` but does not forward it to the inference backend, so the prompt is what actually drives batching. The flag is kept accurate so the request does not contradict the prompt.
   - `max_tokens` caps one model turn (`llm.max_output_tokens`, default 2048) so a runaway completion cannot decode until it exhausts the context window and trip the length nudge.
    - Tool results reach the model untruncated, but a result larger than the per-result context budget is replaced with a `FAILED` result that tells the model to capture or narrow the request. If a chat call still overflows, the largest remaining tool results are discarded and the call is retried once per result; compaction is the backstop for the rest.
   - Injection note: if the model hits the length limit with no tool call, a "be concise" nudge is appended and the loop continues.

7. **Tool dispatch.** `Call` in the `agent` package maps tool names to LXD operations. `vm_exec` first passes the command through `validation.ValidateShellCommand`, so a command outside the allowlist never reaches LXD. `Call` also emits the `agent_tool_call` and `tool_succeeded`/`tool_failed` log events.

8. **Result plumbing.** Tool results are surfaced to the model as JSON (`{"status":"SUCCESS","data":{...}}` or `{"status":"FAILED",...}`) attached to the `tool` role. The combined output of every executed command is collected and printed to stdout at the end, `---`-separated, when the loop finishes.

9. **Teardown.** The kronk runtime is unloaded and the OTel `shutdown` flushes the log batch processor before exit (deferred in `main`).

## Data flow detail (single `vm_exec`)

```mermaid
flowchart TD
    A[Model] -->|tool_choice| B[Agent]
    B -->|"vm_exec: { command: apt update }"| C[Call]
    C --> D[validation.ValidateShellCommand]
    D -->|No| E[FAILED JSON to model]
    D -->|Yes| F["LXD ExecInstance: bash -c <cmd>"]
    F --> G[stdout/stderr back to model]
    E --> B
    G --> B
```
