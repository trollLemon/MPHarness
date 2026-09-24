# MPHarness - Agent Guidelines

## Project Overview
Local harness for running tests in a Multipass VM.

## Structure
- `cmd/mph/` - Main CLI entry point
- `internal/agent/` - Agent management (lifecycle, spec, model)
- `internal/config/` - Configuration handling
- `internal/harness/` - Harness orchestration
- `internal/multipass/` - Multipass VM operations
- `internal/otel/` - OpenTelemetry pipeline (tracer/meter/logger providers, zerolog Hook)
- `internal/validation/` - Input validation

## Commands
```bash
# Build
go build -o bin/mph ./cmd/mph

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
- Multipass interactions via `internal/multipass`
- Agent lifecycle in `internal/agent`

## Code Quality
- Run `go vet ./...` and `golangci-lint run` before committing
- Follow Go idioms and effective Go guidelines
- Keep functions small and focused
- Use meaningful variable and function names
- Minimal comments - code should be self-documenting
- Only add comments for non-obvious workarounds or constraints

## Testing OTel Instrumentation
OTel is disabled by default; when enabled it exports logs, traces, and metrics via OTLP/gRPC (default `localhost:4317`).

### Smoke test (tiny deterministic model)
Uses `test-otel.yaml` (Qwen3-0.6B-Q8_0, `llm.temperature: 0.0` so output is stable across runs) with `otel.enabled: true`:
```bash
make build
./bin/mph ./test-otel.yaml      # requires an OTLP collector on localhost:4317
```
Traces: `mph.run` → `mph.vm.create`/`multipass.launch` → `mph.agent.iteration`×N (with `mph.agent.tool_call`/`tool_result` events and `multipass.exec` children) → `mph.vm.delete`; logs mirror console lines with `trace_id`; metrics like `mph.tokens`, `mph.agent.tool.calls`.

### Negative checks
- Run `./bin/mph ./test.yaml` with collector down. It must complete with no `:4317` dials. This proves default-off.
- Env override: `OTEL_EXPORTER_OTLP_ENDPOINT=host:4317 ./bin/mph --otel ./test-otel.yaml`.
- Flag vs YAML: `--otel` with no `otel:` block enables via defaults; `otel.enabled: true` without flag also enables.

### Enabling
`--otel` flag (or `MPH_OTEL=true`) / YAML `otel.enabled: true` / standard `OTEL_*` env vars. Precedence: flag/env > YAML > defaults (`localhost:4317`, service `mph`). See `docs/how-to/enable_otel.md`.

## Dependencies
- Go 1.27+
- Multipass installed on host
