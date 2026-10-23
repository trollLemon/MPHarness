# Enable OpenTelemetry in MPHarness

OTel is **disabled by default**; when off, `mph` has zero behavioural change and near-zero overhead. When enabled it exports **logs** over OTLP/gRPC (default `localhost:4317`) to any OTLP collector.

This page covers the three ways to enable it, the precedence rules, the bundled observability stack, and how to verify the setup.

## 1. Configuration

### YAML (`otel:` block, optional)

```yaml
otel:
  enabled: false
  endpoint: localhost:4317   # OTLP/gRPC host:port, no scheme
  service_name: mph
  resource_attributes:       # optional, added to the log resource
    environment: dev
```

If the `otel` block is absent, `enabled` defaults to `false` and the endpoint and service name are left empty for `mph.resolveOtelConfig` to fill in. Environment and flag overrides are merged before defaults are applied. See [precedence](#precedence-highest-first).

`resource_attributes` is free-form `map[string]string`; it is merged with `OTEL_RESOURCE_ATTRIBUTES` from the environment by `resource.WithFromEnv()` in `internal/otlp.Setup`.

### CLI flags

```bash
./bin/mph --otel ./testing/test.yaml             # enable
./bin/mph --otel=false ./testing/test.yaml       # disable even if YAML enables it
./bin/mph --otel --pretty ./testing/test.yaml    # readable logs alongside the OTLP export
```

`--otel` is also usable via env:

```bash
MPH_OTEL=true ./bin/mph ./testing/test.yaml
MPH_OTEL=false ./bin/mph ./testing/test.yaml
```

`MPH_OTEL` accepts only `true` or `false` (case-insensitive). Any other value, including blank, fails config loading with an error that names the variable.

### OTel-standard environment variables

The SDK reads these automatically; `mph` also respects them for precedence:

- `OTEL_EXPORTER_OTLP_ENDPOINT` (e.g. `mycollector:4317`). Use a bare `host:port` with no scheme; the exporter is gRPC/insecure, so a `https://` value fails
- `OTEL_SERVICE_NAME`
- `OTEL_RESOURCE_ATTRIBUTES` (e.g. `environment=prod,region=eu`)
- `OTEL_SDK_DISABLED=true` disables the SDK even if `mph` enabled it

`OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` is ignored: `mph` always passes the resolved endpoint explicitly, which overrides it.

### Precedence (highest first)

| Setting | 1st | 2nd | 3rd | 4th |
|---|---|---|---|---|
| enabled | `--otel` | `MPH_OTEL` | YAML `otel.enabled` | `false` |
| endpoint | `OTEL_EXPORTER_OTLP_ENDPOINT` | YAML `otel.endpoint` | `localhost:4317` | |
| service name | `OTEL_SERVICE_NAME` | YAML `otel.service_name` | `mph` | |

`internal/config.LoadFileWithSources` merges YAML, then environment, then explicitly set flags, then applies defaults. The endpoint/service defaults are `otlp.DefaultEndpoint` / `otlp.DefaultServiceName`, applied in `mph.resolveOtelConfig` and again in `internal/otlp.Setup` for programmatic callers that skip the CLI.

`resource_attributes` from YAML and `OTEL_RESOURCE_ATTRIBUTES` from the env are **merged** (env via `resource.WithFromEnv()` plus the YAML map in `internal/otlp.Setup`).

`--otel` with no `otel:` block still enables tracking using env/defaults; no `--otel` with `otel.enabled: true` also enables it; `--otel=false` disables it even when YAML enables it.

## 2. The observability stack

`docker-compose/` holds a Compose stack that receives everything `mph` emits, so you get a UI without wiring a collector yourself. Grafana examples and provisioning live in `example_grafana/`; test inputs and integration scripts live in `testing/`.

```bash
make otel-up    # start the stack
make otel-down  # stop it, keep the data
```

`mph` still points at `localhost:4317`. The stack is a backend, not something you configure `mph` to find.

### Topology

```text
mph ──OTLP/gRPC :4317──▶ otel-collector ──logs──▶ Loki ──▶ Grafana :3000
```

`docker-compose/collector-config.yaml` routes logs that way and also prints everything to the collector's own stdout via the `debug` exporter, which is the fastest way to confirm data is arriving before worrying about a UI.

### Services

| Service | Port | Purpose |
|---|---|---|
| `otel-collector` | 4317 | OTLP/gRPC receiver, forwards logs to Loki |
| `grafana` | 3000 | Dashboards (admin/admin) and Explore |
| `prometheus` | 9090 | Collector self-telemetry only (receiver/exporter health panels); `mph` exports no metrics |
| `loki` | 3100 | Log storage, pushed to by the collector's `loki` exporter |

### Dashboard

`example_grafana/dashboards/mph-otel.json` is provisioned automatically on start, so the **MPHarness - OTel (Logs)** dashboard is in Grafana as soon as the stack is up.

Every run panel is a LogQL query over structured log events. Each dashboard-facing log line carries an `event` field; the Loki exporter nests it under `attributes`, so `| json` exposes it as `attributes_event`:

| `event` | Key fields |
|---|---|
| `token_usage` | `iter`, `prompt_tokens`, `completion_tokens`, `total_tokens`, `tps`, `cached_tokens`, `reasoning_tokens`, `ttft_ms`, `ctx_window` |
| `iteration` | `iter`, `dur_ms`, `chat_ms`, `tool_ms` |
| `agent_tool_call` | `iter`, `tool`, `id`, `args` |
| `tool_succeeded` / `tool_failed` | `iter`, `tool`, `id`, `dur_ms`, and `out` or `error` |
| `tool_output_too_large` | `iter`, `tool`, `id`, `bytes`, `budget` |
| `context_overflow` | `iter`, `outcome`, `replaced`, `reclaimed_bytes`, `prompt_tokens`, `ctx_window`, `error` |
| `context_compaction` | `iter`, `outcome`, `tok_before`, `tok_after`, `reclaimed`, `thr`, `summary`, `handles`, `compacted`, `try`, `dur_ms`, and `error` on failure |
| `context_compaction_applied` | `iter`, `compacted` (the rewritten conversation) |
| `run_summary` | `outcome`, `status`, `reason`, `iterations`, `compactions`, `dur_ms`, `avg_iter_ms`, `prompt_tokens`, `completion_tokens`, `reclaimed_tokens`, `tool_calls`, `tool_failures`, `ctx_window` |

`outcome` is one of `ok`, `failed`, `error`, `budget_exhausted`, or `timeout`. `status` and `reason` are the agent's terminal `task_status` values.

For example, prompt tokens per iteration of one run:

```logql
max by (attributes_iter) (max_over_time({instance="<name>:<uuid>"} | json
  | attributes_event="token_usage" | unwrap attributes_prompt_tokens [$__range]))
```

The **Run** selector scopes the summary, per-iteration timing and tokens,
context, tools, and narrative panels to one run. The comparison row retains
all runs in the selected time range. Runs are selected by the Loki `instance`
stream label (the `service.instance.id` resource), which combines the run
name and UUID, so repeated runs with the same name stay distinct.

**Context over time** plots context used and remaining context (window minus
used) from each `token_usage` event, with applied compactions as points. Its time
axis spans the run itself, not the dashboard range; the range only has to
include the run. Token counts remain available, but there are no pricing controls or
monetary cost calculations in the dashboard or application.

### Data retention

The three stateful services use named volumes (`loki-data`, `prometheus-data`,
and `grafana-data`), so `make otel-down` and `make otel-up` preserve their
data, subject to each backend's retention settings.

To wipe only this stack's stored data and start clean:

```bash
docker compose -f docker-compose/docker-compose.yaml down --volumes
make otel-up
```

## 3. Smoke test

`testing/test.yaml` is the canonical smoke config: a pinned small instruct model (`unsloth/granite-4.0-350m-IQ4_XS`), `llm.temperature: 0.0` (deterministic, so logs are comparable run to run), and no `otel:` block. OTel is enabled with the `--otel` flag:

```bash
make build
make otel-up                # OTLP receiver must be listening
./bin/mph --otel ./testing/test.yaml
```

Then:

- Grafana on <http://localhost:3000> provisions the **MPHarness - OTel (Logs)** dashboard, or use Explore → Loki
- In Loki the run is identified by the `<name>:<uuid>` `instance` stream label; the **Run** selector uses that label
- The log stream should show `iteration`, `token_usage`, tool events, and a final `run_summary` event

Model replies land in Loki under the body `model output`, split by `part` (`content` or `reasoning`) with the payload in `text`. Nothing is logged for an iteration that produced neither, which is normal for a turn that is only a tool call.

### Negative checks

```bash
# 1. Disabled by default: with the stack down, nothing should dial :4317
./bin/mph ./testing/test.yaml       # this config has no otel block

# 2. Env overrides endpoint and service name
OTEL_EXPORTER_OTLP_ENDPOINT=mycollector:4317 ./bin/mph --otel ./testing/test.yaml
OTEL_SERVICE_NAME=custom-mph ./bin/mph --otel ./testing/test.yaml

# 3. Flag vs YAML
./bin/mph --otel ./testing/test.yaml     # no otel block, flag enables via env/defaults
./bin/mph ./testing/output.yaml          # YAML otel.enabled: true, no flag - also enabled
```

Check 1 is the one that matters most: it is what proves the default-off promise, so run it with the collector actually stopped.

### Long runs

`testing/longrun.yaml` is the same shape with a 30-step checklist, `max_iterations: 60`, and a 90m total timeout, so a run takes long enough to watch iteration duration, tokens-per-second, and context usage climb on the dashboard rather than flashing past:

```bash
./bin/mph ./testing/longrun.yaml
```

It uses a separate VM name (`mph-longrun-3`) and `service_name` (`mph-longrun-3`).
Both land on the same dashboard; use the **Run** selector for details and
the **All runs** table for comparison.

## 4. Tuning what gets exported

Attribute payloads are capped so one large tool result cannot swamp the collector or bury the surrounding signal. The limits live in a top-level `truncation:` block:

```yaml
truncation:
  command_output: 600    # vm_exec output echoed into logs
  log_content: 8000      # model reasoning/content and the final answer
  tool_result: 4000      # tool arguments and results in logs
  nudge: 2000            # length-limit nudge replayed back to the model
```

Any value of `0` or less is replaced by the default in `config.DefaultCommandOutputBytes`, `DefaultLogContentBytes`, `DefaultToolResultBytes`, and `DefaultNudgeBytes`, so an omitted block behaves exactly as before.

`tool_result` caps the tool arguments on `agent_tool_call` events and the tool result echoed into `tool_succeeded` / `tool_failed` events.

## 5. Debugging tips

- `mph -k ./testing/test.yaml` keeps the VM after the run for inspection.
- `--pretty` gives readable console logs alongside the OTLP export, which is usually enough to diagnose without opening Grafana.
- Nothing arriving at all? `docker compose -f docker-compose/docker-compose.yaml logs otel-collector` shows the `debug` exporter output.
- Logs look empty? The exporter batches for about a second, so give it a moment after the run finishes.
- Check the receiver is up before suspecting `mph`: `ss -ltnp | grep 4317`.
- OTel SDK debug: `OTEL_LOG_LEVEL=debug` shows exporter retries; `OTEL_SDK_DISABLED=true` forces the whole thing off.

## 6. Where it lives in code

- `internal/otlp/otlp.go`: `Config`, `Setup` (creates the `otlploggrpc` exporter and logger provider, installs `global.SetLoggerProvider`), returns `shutdown`, `Hook` (zerolog → `otellog.Record`), and `LogEvent` (structured fields to both zerolog and OTLP).
- `internal/config/config.go`: `OtelConfig`, `TruncationConfig`, YAML/env/flag merging via `LoadFileWithSources`, and the default constants.
- `mph/main.go`: `--otel` flag, `resolveOtelConfig` (defaults and `otlp.Config` mapping), `otlp.Setup`, and `log.Logger.Hook(NewHook("mph"))` with a deferred shutdown.
- `internal/lxd/lxd.go`: LXD operations and command execution; no tracing.
- `internal/harness/harness.go`: VM lifecycle orchestration; no tracing.
- `internal/agent/agent.go`: `Execute` (ctx threaded from harness), the length-limit nudge, and the `token_usage`/`iteration`/tool log events.
- `internal/agent/compaction.go`: the `context_compaction`/`context_compaction_applied` events.
- `internal/agent/run_summary.go`: the run outcome and the `run_summary` event.
- `internal/agent/model.go`: `zerologAdapter` maps kronk SDK messages onto zerolog.

## Related

- [Software architecture](../reference/arch.md): package map including `internal/otlp`.
- [Configure the LLM](configure_llm.md): `temperature: 0.0` for deterministic smoke.
- [Good models](../reference/good_models.md): picking small models for the smoke test.
