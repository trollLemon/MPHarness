# MPHarness - Agent Guidelines

## Project Overview
Local harness for running tests in an LXD VM.

## Structure
- `mph/` - Main CLI entry point
- `internal/agent/` - Agent management (lifecycle, spec, model)
- `internal/config/` - Configuration handling
- `internal/harness/` - Harness orchestration
- `internal/lxd/` - LXD VM operations
- `internal/otlp/` - OTLP log pipeline (logger provider, zerolog Hook)
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
- Configuration via `internal/config` (koanf: YAML, then existing env vars, then explicit flags, then defaults)
- LXD interactions via `internal/lxd`
- Agent lifecycle in `internal/agent`

## Code Quality
- Run `go vet ./...` and `golangci-lint run` before committing
- Follow Go idioms and effective Go guidelines
- Keep functions small and focused
- Avoid turning one cohesive function into a chain of single-use helpers; extract only for reuse or materially simpler control flow
- All test files use external test packages (`package <name>_test`), including `mph` (`package main_test`)
- Test through the exported API. `internal/agent` is tested entirely through `NewAgent`/`Execute`/`Call` and the chat requests and log events a run produces; do not add an `export_test.go` there
- `export_test.go` re-exports are a last resort for packages whose behavior has no public entry point; keep the list to symbols a test actually uses
- Do not open or read the same file twice in one operation; reuse the first read
- Use meaningful variable and function names
- Minimal comments - code should be self-documenting
- Only add comments for non-obvious workarounds or constraints
- Search for an existing helper before adding one; do not duplicate parsing, validation, or formatting logic
- Do not keep exported functions or methods that only tests use; put test helpers in `_test.go`
- Avoid redundant casts and conversions; let the declared types do the work
- Keep one source of truth for defaults and derive help text or docs from it
- Do not use em dashes or en dashes in documentation. A sentence that wants one is usually too long: split it, or use a semicolon, colon, or parentheses.
- Any comments should only exist to convey unclear meaning of something, never including the context of something else. For example, a bad comment is something like:
 
  // os.Root refuses to follow symlinks that escape the tree, so a file
  // swapped for a symlink mid-walk cannot redirect the read.
  root, err := os.OpenRoot(clean)

## Testing OTel Instrumentation
OTel is disabled by default; when enabled it exports logs via OTLP/gRPC (default `localhost:4317`). There are no OTel traces or metrics: dashboards use LogQL over structured log events.

### Smoke test (tiny deterministic model)
Uses `testing/test.yaml` (`unsloth/granite-4.0-350m-IQ4_XS`, `llm.temperature: 0.0` so output is stable across runs); the config has no `otel:` block, so enable with `--otel`:
```bash
make build
make otel-up                       # Compose stack under docker-compose/, OTLP on localhost:4317
./bin/mph --otel ./testing/test.yaml
```
The smoke test runs a real LXD VM, so the host needs a working LXD daemon and default profile.
Dashboard data comes from log lines tagged `event=` (`token_usage`, `iteration`, `agent_tool_call`, `tool_succeeded`, `tool_failed`, `tool_output_too_large`, `context_overflow`, `context_compaction`, `context_compaction_applied`, `run_summary`). The `<name>:<uuid>` `instance` label identifies a run in Loki.

### Negative checks
- Run `./bin/mph ./testing/test.yaml` with collector down. It must complete with no `:4317` dials. This proves default-off.
- Env override: `OTEL_EXPORTER_OTLP_ENDPOINT=host:4317 ./bin/mph --otel ./testing/test.yaml`.
- Flag vs YAML: `--otel` with no `otel:` block enables via defaults; `otel.enabled: true` (e.g. `testing/output.yaml`) without flag also enables; `--otel=false` overrides YAML.

### Enabling
`--otel` (explicit true/false) / `MPH_OTEL` / YAML `otel.enabled` / standard `OTEL_*` env vars. Precedence: `--otel` > `MPH_OTEL` > YAML > defaults (`localhost:4317`, service `mph`). `MPH_RUN_NAME` overrides `run.name`. See `docs/how-to/enable_otel.md`.

## Dependencies
- Go 1.27.2+
- LXD 5.x LTS or newer installed on host (deb or snap), daemon running, and the user in the `lxd` group
- VM images that include the LXD agent (official Ubuntu 20.04+ images do; 18.04 requires a manual agent install)
- A working default profile from `lxd init` (root disk on a storage pool, network)
