# MPHarness - Agent Guidelines

## Project Overview
Local harness for running tests in an LXD VM.

## Structure
- `mph/` - Main CLI entry point
- `internal/agent/` - Agent management (lifecycle, spec, model)
- `internal/config/` - Configuration handling
- `internal/harness/` - Harness orchestration
- `internal/lxd/` - LXD VM operations
- `internal/otel/` - OpenTelemetry pipeline (tracer/logger providers, zerolog Hook)
- `internal/validation/` - Input validation

## Commands
```bash
# Build
go build -o bin/mph ./mph

# Test
go test ./...

# Lint
go vet ./...
golangci-lint run

# Run locally
./bin/mph --help
```

## Key Patterns
- Use `internal/` packages for non-exported code
- Tests live alongside source files (`*_test.go`)
- Write table-driven tests where possible
- Configuration via `internal/config`
- LXD interactions via `internal/lxd`
- Agent lifecycle in `internal/agent`

## Code Quality
- Run `go vet ./...` and `golangci-lint run` before committing
- Follow Go idioms and effective Go guidelines
- Keep functions small and focused
- Use meaningful variable and function names
- Minimal comments - code should be self-documenting
- Only add comments for non-obvious workarounds or constraints
- Do not use em dashes or en dashes in documentation. A sentence that wants one is usually too long: split it, or use a semicolon, colon, or parentheses.

## Testing OTel Instrumentation
OTel is disabled by default; when enabled it exports logs and traces via OTLP/gRPC (default `localhost:4317`). There are no OTel metrics: dashboards use LogQL over structured log events.

### Smoke test (tiny deterministic model)
Uses `testing/test.yaml` (`unsloth/granite-4.0-350m-IQ4_XS`, `llm.temperature: 0.0` so output is stable across runs); the config has no `otel:` block, so enable with `--otel`:
```bash
make build
make otel-up                       # Compose stack under docker-compose/, OTLP on localhost:4317
./bin/mph --otel ./testing/test.yaml
```
The smoke test runs a real LXD VM, so the host needs a working LXD daemon and default profile.
Traces: `mph.run` → `mph.vm.create`/`vm.launch` → `mph.agent.iteration`×N (with `mph.agent.tool_call`/`tool_result` events and `vm.exec` children) → `mph.vm.delete`; logs mirror console lines with `trace_id`; dashboard data comes from log lines tagged `event=` (`token_usage`, `iteration`, `agent_tool_call`, `tool_succeeded`, `tool_failed`, `tool_output_too_large`, `context_overflow`, `context_compaction`, `context_compaction_applied`, `run_summary`).

### Negative checks
- Run `./bin/mph ./testing/test.yaml` with collector down. It must complete with no `:4317` dials. This proves default-off.
- Env override: `OTEL_EXPORTER_OTLP_ENDPOINT=host:4317 ./bin/mph --otel ./testing/test.yaml`.
- Flag vs YAML: `--otel` with no `otel:` block enables via defaults; `otel.enabled: true` (e.g. `testing/output.yaml`) without flag also enables.

### Enabling
`--otel` flag (or `MPH_OTEL=true`) / YAML `otel.enabled: true` / standard `OTEL_*` env vars. Precedence: flag/env > YAML > defaults (`localhost:4317`, service `mph`). See `docs/how-to/enable_otel.md`.

## Dependencies
- Go 1.27.2+
- LXD 5.x LTS or newer installed on host (deb or snap), daemon running, and the user in the `lxd` group
- VM images that include the LXD agent (official Ubuntu 20.04+ images do; 18.04 requires a manual agent install)
- A working default profile from `lxd init` (root disk on a storage pool, network)
