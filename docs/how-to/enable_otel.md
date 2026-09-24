# Enable OpenTelemetry in MPHarness

OTel is **disabled by default**; when off, `mph` has zero behavioral change and ~zero overhead. When enabled it exports **traces, logs, and metrics** via OTLP/gRPC (default `localhost:4317`) to any OTLP collector.

This page covers the three ways to enable it, the precedence rules, and how to verify it with the smoke test.

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

Defaults are applied in `internal/config.Parse` just like the existing `vm.name` default: if `otel` is absent, it defaults to `enabled: false`, `endpoint: localhost:4317`, `service_name: mph`.

`resource_attributes` is free-form `map[string]string`; it is merged with `OTEL_RESOURCE_ATTRIBUTES` from the environment by `resource.WithFromEnv()` in `internal/otel.Setup`.

### CLI flags

```bash
./bin/mph --otel ./test-otel.yaml     # enable (bool)
./bin/mph --otel --pretty ./test.yaml # readable logs + OTel export
```

`--otel` is also usable via env:

```bash
MPH_OTEL=true ./bin/mph ./test-otel.yaml
MPH_OTEL=1 ./bin/mph ./test-otel.yaml
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

`resource_attributes` from YAML and `OTEL_RESOURCE_ATTRIBUTES` from the env are **merged** (env via `resource.WithFromEnv()` plus YAML map in `internal/otel.Setup`).

`--otel` with no `otel:` block still enables tracking using env/defaults; no `--otel` and `otel.enabled: true` also enables it.

## 2. Smoke test (tiny deterministic model)

`test-otel.yaml` at the repo root is the canonical smoke config: `unsloth/Qwen3-0.6B-Q8_0` (~0.7 GB), `llm.temperature: 0.0` (deterministic, comparable traces run-to-run), `otel.enabled: true`:

```bash
make build
./bin/mph ./test-otel.yaml      # requires an OTLP collector on localhost:4317
```

When enabled, `mph` exports OTLP/gRPC to `localhost:4317` (traces `mph.run` → `mph.vm.create`/`multipass.launch` → `mph.agent.iteration` with `tool_call`/`tool_result` events, logs with `trace_id`, metrics `mph.tokens` etc.).

## 3. Negative and env-override checks

```bash
# 1. Disabled by default – collector down must not be dialed
make otel-collector-down
./bin/mph ./test.yaml           # default test.yaml has no otel block
# should complete with no :4317 connection attempts and no OTel hook

# 2. Env overrides endpoint / service name
OTEL_EXPORTER_OTLP_ENDPOINT=mycollector:4317 ./bin/mph --otel ./test-otel.yaml
OTEL_SERVICE_NAME=custom-mph ./bin/mph --otel ./test-otel.yaml  # overrides YAML mph-smoke

# 3. Flag vs YAML
./bin/mph --otel ./test.yaml              # no otel block, flag enables via env/defaults
./bin/mph ./test-otel.yaml                # YAML enabled:true, no flag – also enabled
```

## 4. Debugging tips

- `mph -k ./test-otel.yaml` keeps the VM after the run for inspection.
- `--pretty` for human-readable console logs alongside the OTLP export.
- If no spans appear, check the collector is listening and the exporter's periodic flush has run (default 5 s batch):
  ```bash
  ss -ltnp | grep 4317
  curl -s http://localhost:4317  # collector health (gRPC)
  ```
- OTel SDK debug: `OTEL_LOG_LEVEL=debug` (traces exporter retries) or set `OTEL_SDK_DISABLED=true` to force off.
- `mph.prompt` on `mph.run` is truncated to 8000 chars; `mph.command` to 4000. Adjust in `internal/harness`/`internal/multipass` if sensitive data is a concern.

## 5. Where it lives in code

- `internal/otel/otel.go`: `Config`, `Setup` (creates `otlptracegrpc`/`otlpmetricgrpc`/`otlploggrpc`, `TracerProvider`/`MeterProvider`/`LoggerProvider`, `global.SetLoggerProvider`), returns `shutdown`, and `Hook` (zerolog → `otellog.Record`, trace correlation via `e.GetCtx()`).
- `internal/config/config.go`: `OtelConfig`, YAML `otel:` parsing, defaults in `Parse`.
- `cmd/mph/main.go`: `--otel` flag, `MPH_OTEL` env, precedence merge (`resolveOtelConfig`), `otel.Setup` + `log.Logger.Hook(NewHook("mph"))`, `defer shutdown`.
- `internal/multipass/multipass.go`: span per command (`multipass.*`).
- `internal/harness/harness.go`: root `mph.run` + `mph.vm.create`/`delete` spans.
- `internal/agent/agent.go`: `Execute(ctx, ...)` (ctx threaded from harness), `mph.agent.iteration` spans, `tool_call`/`tool_result`/`length_nudge` events, `mph.tokens`/`mph.agent.tool.*` metrics.
- `internal/agent/model.go`: `zerolog.Ctx(ctx)` in `zerologAdapter` for log↔trace linkage, `mph.model.init`/`mph.model.load` spans (`mph.context_window` is `"auto"` when `0`).

## Related

- [Software architecture](../reference/arch.md) – package map including `internal/otel`.
- [Configure the LLM](configure_llm.md) – `temperature: 0.0` for deterministic smoke.
- [Good models](../reference/good_models.md) – picking small models for the smoke test.
