# Enable OpenTelemetry in MPHarness

OTel is **disabled by default**; when off, `mph` has zero behavioural change and near-zero overhead. When enabled it exports **traces, logs, and metrics** over OTLP/gRPC (default `localhost:4317`) to any OTLP collector.

This page covers the three ways to enable it, the precedence rules, the bundled observability stack, and how to verify both.

## 1. Configuration

### YAML (`otel:` block, optional)

```yaml
otel:
  enabled: false
  endpoint: localhost:4317   # OTLP/gRPC host:port, no scheme
  service_name: mph
  resource_attributes:       # optional, added to every signal as resource attrs
    environment: dev
```

If the `otel` block is absent, `enabled` defaults to `false` and the endpoint and service name are left empty for `cmd/mph.resolveOtelConfig` to fill in. See [precedence](#precedence-highest-first) for why the default-off default lives in one place rather than two.

`resource_attributes` is free-form `map[string]string`; it is merged with `OTEL_RESOURCE_ATTRIBUTES` from the environment by `resource.WithFromEnv()` in `internal/otel.Setup`.

### CLI flags

```bash
./bin/mph --otel ./test.yaml        # enable (bool)
./bin/mph --otel --pretty ./test.yaml  # readable logs alongside the OTLP export
```

`--otel` is also usable via env:

```bash
MPH_OTEL=true ./bin/mph ./test.yaml
MPH_OTEL=1 ./bin/mph ./test.yaml
```

Any of `true`/`1`/`yes` (case-insensitive) counts as enabled.

### OTel-standard environment variables

The SDK reads these automatically; `mph` also respects them for precedence:

- `OTEL_EXPORTER_OTLP_ENDPOINT` (e.g. `mycollector:4317` or `https://collector:4317`)
- `OTEL_SERVICE_NAME`
- `OTEL_RESOURCE_ATTRIBUTES` (e.g. `environment=prod,region=eu`)
- `OTEL_SDK_DISABLED=true` disables the SDK even if `mph` enabled it

`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` / `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` also work per-signal if you need them.

### Precedence (highest first)

| Setting | 1st | 2nd | 3rd |
|---|---|---|---|
| enabled | `--otel` / `MPH_OTEL` | YAML `otel.enabled` | `false` |
| endpoint | `OTEL_EXPORTER_OTLP_ENDPOINT` | YAML `otel.endpoint` | `localhost:4317` |
| service name | `OTEL_SERVICE_NAME` | YAML `otel.service_name` | `mph` |

The third column is `mphotel.DefaultEndpoint` / `DefaultServiceName`, applied in `cmd/mph.resolveOtelConfig` for the YAML path and again in `internal/otel.Setup` for programmatic callers that skip the CLI.

`resource_attributes` from YAML and `OTEL_RESOURCE_ATTRIBUTES` from the env are **merged** (env via `resource.WithFromEnv()` plus the YAML map in `internal/otel.Setup`).

`--otel` with no `otel:` block still enables tracking using env/defaults; no `--otel` with `otel.enabled: true` also enables it.

## 2. The observability stack

`testing/` holds a Compose stack that receives everything `mph` emits, so you get a UI without wiring a collector yourself.

```bash
make otel-up    # start the stack
make otel-down  # stop it, keep the data
```

`mph` still points at `localhost:4317` — the stack is a backend, not something you configure `mph` to find.

### Topology

```text
mph ──OTLP/gRPC :4317──▶ otel-collector
                           ├── traces ──▶ Tempo      ◀── Grafana :3000
                           ├── logs   ──▶ Loki       ┘
                           └── metrics ─▶ Prometheus ┘
```

`testing/collector-config.yaml` routes each signal that way and also prints everything to the collector's own stdout via the `debug` exporter, which is the fastest way to confirm data is arriving before worrying about a UI.

Tempo replaced Jaeger here. Grafana's Jaeger datasource sends the search window in milliseconds while Jaeger's API expects microseconds, so every search resolved to 1970 and returned nothing at all, with no error to explain it.

### Services

| Service | Port | Purpose |
|---|---|---|
| `otel-collector` | 4317 | OTLP/gRPC receiver, fans signals out |
| `grafana` | 3000 | Dashboards (admin/admin) and Explore |
| `tempo` | 3200 | Trace search backend, also a usable trace UI of its own |
| `prometheus` | 9090 | Metric storage, scraped by the collector's `prometheus` exporter |
| `loki` | 3100 | Log storage, pushed to by the collector's `loki` exporter |

### Dashboard

`testing/grafana/dashboards/mph-otel.json` is provisioned automatically on start, so the **MPH OTel** dashboard is in Grafana as soon as the stack is up.

It is laid out in rows so a row is useful before the one below it has data:

1. **Run summary** — tool calls, tool failures, tool duration p95, average tokens/sec for the selected range
2. **Agent metrics** — token usage, iterations, and per-tool counts and durations
3. **Per run totals** — a table with one row per run id: tokens, iterations, tool calls, context used
4. **Per run over time** — one line per run id for each of those metrics
5. **OTel collector** — points accepted per interval, and export failures (which should stay at zero)
6. **Logs** — all lines, per-interval counts by level, warnings and errors, and lines with trace correlation
7. **Traces** — TraceQL search against Tempo

There are deliberately **no dashboard variables**: every panel shows all runs in the selected time range, so nothing is hidden behind a filter that silently matches nothing. The rules the panels follow:

- A per-run line **ends with its run** rather than running on flat. Raw counters are drawn as-is with `spanNulls: false`, so a finished run simply stops.
- A counter that can only climb is drawn as a **burst** instead: tokens are shown as `increase(...)` per interval, so a run reads as a hump that returns to zero once it stops working.
- **Cumulative totals live in the table**, not on a line, because a cumulative line freezes every finished run at its final value and a wall of those is unreadable.

### Searching traces by hand

Tempo's own search API takes `start` and `end` in **seconds**, and only together — passing milliseconds returns `400 invalid start`, and passing one without the other is also a `400`. `tags` and the TraceQL `q` parameter are mutually exclusive.

```bash
curl -sG http://localhost:3200/api/search \
  --data-urlencode 'q={ resource.service.name =~ ".+" }' \
  --data-urlencode "start=$(( $(date +%s) - 3600 ))" --data-urlencode "end=$(date +%s)"
```

TraceQL has to match against **flushed blocks**, not just live data, so `testing/tempo.yaml` sets `storage.trace.block.version: vParquet4`; without it a query that works while a run is in flight returns nothing afterwards.

`testing/docker-compose.lgtm.yaml` is an alternative single-image `otel-lgtm` stack for when the five-service version is more than you need.

### Data retention

The three stateful services use named volumes — `loki-data`, `prometheus-data`, and `grafana-data` — so `make otel-down` and `make otel-up` keeps your logs and dashboards. Metrics live only as long as Prometheus's retention window; traces are ephemeral by design, since they are meant to be read while a run is in flight.

To wipe everything and start clean:

```bash
docker compose -f testing/docker-compose.yaml --profile none down -v
```

## 3. Smoke test

`testing/test.yaml` is the canonical smoke config: a pinned small instruct model, `llm.temperature: 0.0` (deterministic, so traces are comparable run to run), and `otel.enabled: true` with `service_name: mph-smoke`.

```bash
make build
make otel-up                # OTLP receiver must be listening
./bin/mph ./testing/test.yaml
```

Then:

- Grafana on <http://localhost:3000> — the **MPH OTel** dashboard, or Explore → Tempo/Loki/Prometheus
- The trace shape to expect is `mph.run` → `mph.vm.create` / `multipass.launch` → `mph.agent.iteration` × N (with `mph.agent.tool_call` / `tool_result` events and a `multipass.exec` child per command) → `mph.vm.delete`

`mph.run.id` is a UUID on every span, event, and metric, so one run can be picked out of a shared dashboard. Logs carry `trace_id` and `span_id`, so a log line links back to the trace it belongs to.

Model replies land in Loki under the body `model output`, split by `part` (`content` or `reasoning`) with the payload in `text`. Nothing is logged for an iteration that produced neither, which is normal for a turn that is only a tool call.

### Negative checks

```bash
# 1. Disabled by default: with the stack down, nothing should dial :4317
./bin/mph ./test.yaml       # this config has no otel block

# 2. Env overrides endpoint and service name
OTEL_EXPORTER_OTLP_ENDPOINT=mycollector:4317 ./bin/mph --otel ./testing/test.yaml
OTEL_SERVICE_NAME=custom-mph ./bin/mph --otel ./testing/test.yaml

# 3. Flag vs YAML
./bin/mph --otel ./test.yaml           # no otel block, flag enables via env/defaults
./bin/mph ./testing/test.yaml          # YAML enabled:true, no flag - also enabled
```

Check 1 is the one that matters most: it is what proves the default-off promise, so run it with the collector actually stopped.

### Long runs

`testing/longrun.yaml` is the same shape with a 30-step checklist, `max_iterations: 60`, and a 90m total timeout, so a run takes long enough to watch iteration duration, tokens-per-second, and the context gauges climb on the dashboard rather than flashing past:

```bash
./bin/mph ./testing/longrun.yaml
```

It uses a separate VM name (`mph-longrun`) and `service_name` (`mph-longrun`), so it never collides with the smoke test. Both land on the same unfiltered dashboard, so pick a run out of the **Per run totals** table rather than filtering panels.

### If the trace panel looks empty

Grafana 12.2 runs Tempo's TraceQL search in the browser, through the datasource proxy, and its server-side query endpoint has no implementation: posting a `traceql` search to `/api/ds/query` always fails with `backend TraceQL search queries are not supported`, whatever the query, and no request ever reaches Tempo. That makes the panel impossible to verify by scripting the API — it has to be checked in the browser, or through **Explore → Tempo**, which uses the working path. Tempo's own UI on <http://localhost:3200> is the other fallback.

## 4. Tuning what gets exported

Attribute payloads are capped so one large tool result cannot swamp the collector or bury the surrounding signal. The limits live in a top-level `truncation:` block:

```yaml
truncation:
  command_output: 600    # multipass_exec output echoed into logs
  log_content: 8000      # model reasoning/content and the final answer
  tool_result: 4000      # tool arguments and results in spans and logs
  nudge: 2000            # length-limit nudge replayed back to the model
```

Any value of `0` or less is replaced by the default in `config.DefaultCommandOutputBytes`, `DefaultLogContentBytes`, `DefaultToolResultBytes`, and `DefaultNudgeBytes`, so an omitted block behaves exactly as before.

`tool_result` is the span-payload tier, so it also caps the tool arguments on `mph.agent.tool_call` events and the command echoed into the `multipass.exec` span.

## 5. Debugging tips

- `mph -k ./testing/test.yaml` keeps the VM after the run for inspection.
- `--pretty` gives readable console logs alongside the OTLP export, which is usually enough to diagnose without opening Grafana.
- Nothing arriving at all? `docker compose -f testing/docker-compose.yaml logs otel-collector` shows the `debug` exporter output.
- Traces look empty? The exporter batches for ~5s, so give it a moment after the run finishes.
- Check the receiver is up before suspecting `mph`: `ss -ltnp | grep 4317`.
- OTel SDK debug: `OTEL_LOG_LEVEL=debug` shows exporter retries; `OTEL_SDK_DISABLED=true` forces the whole thing off.

## 6. Where it lives in code

- `internal/otel/otel.go` — `Config`, `Setup` (creates the `otlptracegrpc`/`otlpmetricgrpc`/`otlploggrpc` exporters, the providers, and `global.SetLoggerProvider`), returns `shutdown`, and `Hook` (zerolog → `otellog.Record` with trace correlation via `e.GetCtx()`).
- `internal/config/config.go` — `OtelConfig`, `TruncationConfig`, YAML `otel:`/`truncation:` parsing, and the default constants.
- `cmd/mph/main.go` — `--otel` flag, `MPH_OTEL` env, `resolveOtelConfig` (the precedence merge above), `otel.Setup`, and `log.Logger.Hook(NewHook("mph"))` with a deferred shutdown.
- `internal/multipass/multipass.go` — a span per command (`multipass.*`); `Exec` takes the cap for its command echo attribute.
- `internal/harness/harness.go` — the root `mph.run` span plus `mph.vm.create`/`delete`.
- `internal/agent/agent.go` — `Execute` (ctx threaded from harness), `mph.agent.iteration` spans, and the length-limit nudge.
- `internal/agent/agent_otel.go` — all agent metrics, span events, and the run-ID context.
- `internal/agent/model.go` — `zerolog.Ctx(ctx)` in `zerologAdapter` for log↔trace linkage, and the `mph.model.init` / `mph.model.load` spans.

## Related

- [Software architecture](../reference/arch.md) – package map including `internal/otel`.
- [Configure the LLM](configure_llm.md) – `temperature: 0.0` for deterministic smoke.
- [Good models](../reference/good_models.md) – picking small models for the smoke test.
