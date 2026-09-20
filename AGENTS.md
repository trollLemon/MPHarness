# MPHarness - Agent Guidelines

## Project Overview
Local harness for running tests in a Multipass VM.

## Structure
- `cmd/mph/` - Main CLI entry point
- `internal/agent/` - Agent management (lifecycle, spec, model)
- `internal/config/` - Configuration handling
- `internal/harness/` - Harness orchestration
- `internal/multipass/` - Multipass VM operations
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

## Dependencies
- Go 1.27+
- Multipass installed on host
