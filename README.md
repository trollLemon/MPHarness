# MPHarness

Local harness for running agent-driven tasks inside an LXD VM, without the worry of an agent damaging your host.

`mph` lets an LLM drive a tool-calling loop to complete a task within an Ubuntu VM, then hands the results back and tears the VM down.

## How it works

1. **Config load**: YAML, environment, and flags are merged, validated, and defaulted (`mph-vm`, `max_iterations: 10`, timeouts, …).
2. **Model init**: llama.cpp runtimes + model (e.g. `unsloth/Qwen3-0.6B-Q8_0`) are downloaded once and cached, then loaded locally.
3. **VM lifecycle**: the named LXD VM is launched (or reused with `-i`), optional `content_dir` is copied to `/home/ubuntu/content`.
4. **Agent loop**: the LLM reasons, calls tools, reads results, and repeats until it calls `task_status` or hits `max_iterations`.
5. **Teardown**: combined command output is printed (`---`-separated) and the VM is deleted unless `-k` was passed.

Every command the agent tries to run is parsed as shell AST and checked against `allowed_commands` (fail-closed), so anything outside the allowlist never reaches the VM. See [Restrict commands](docs/how-to/restrict_commands.md).

## Prerequisites

- [LXD](https://linuxcontainers.org/lxd/) 5.x LTS or newer installed (deb or snap) with a running daemon
- VM images must include the LXD agent (official Ubuntu 20.04+ images do; 18.04 images require a manual LXD agent install)
- Access to the LXD socket, typically by being in the `lxd` group
- A working default profile from `lxd init` (a root disk on a storage pool, plus a network)
- Go 1.27.2+ (only needed to build from source)
- A few GB free disk (VM image + model + llama.cpp runtimes)
- Hardware that can run a small LLM (CPU works for 0.6B to 1.7B models; a GPU helps for larger ones)


## Install
```bash
go install github.com/trollLemon/MPHarness/mph@latest
```

## Quickstart

Create `first-task.yaml`:

```yaml
vm:
  name: mph-tutorial
  disk: 10G
  ram: 2G
  cpu: 1

model: unsloth/Qwen3-0.6B-Q8_0

llm:
  temperature: 0.2
  top_p: 0.95
  top_k: 40

agent:
  max_iterations: 10
  chat_timeout: 30m
  total_timeout: 30m

prompt: |
  Inside the VM, do the following:
  1. Run ls /etc
  2. Run uname -a
  3. Summarise the results.
```

Run it:

```bash
./bin/mph first-task.yaml
# or
./bin/mph -c first-task.yaml -v --pretty
```

First run downloads the runtimes + model (cached afterwards). Follow the [tutorial](docs/tutorial/run_an_agent_with_task.md) for a full walkthrough.

## Configuration

All run config lives in one YAML file. Only `vm.*`, `model`, and `prompt` are required; everything else has sane defaults.

```yaml
vm:
  name: mph-vm        # optional, default mph-vm; DNS hostname rules, no underscores
  disk: 20G
  ram: 4G
  cpu: 2
  image: noble       # optional Ubuntu image, default ubuntu:24.04

model: unsloth/Qwen3-0.6B-Q8_0
prompt: "test the build"

content_dir: ./my-build   # optional: copied to /home/ubuntu/content in the VM

allowed_commands:         # optional: omit = allow everything (shell binaries always blocked)
  - coreUtils             # meta-entry expanding to GNU coreutils
  - apt
  - git

llm:                      # optional sampling params
  temperature: 0.2
  top_p: 0.95
  top_k: 40
  context_window: 0       # 0 = auto-tune

agent:                    # optional loop bounds
  max_iterations: 10
  chat_timeout: 5m
  total_timeout: 30m

secrets:                  # optional, specify which backend to use for resolving secrets
  source: env             # env|keyring
```

Full reference: `mph --help` lists the config keys, but with illustrative values rather than defaults. See the guides for the real defaults. Guides:

- [Configure the VM](docs/how-to/configure_vm.md) · [Configure the LLM](docs/how-to/configure_llm.md) · [Good models](docs/reference/good_models.md)
- [Restrict commands](docs/how-to/restrict_commands.md) · [Provide content](docs/how-to/provide_content.md)
- [Pass secrets](docs/how-to/use_secrets.md) · [Manage VM lifecycle](docs/how-to/manage_vm_lifecycle.md)

## CLI

```bash
mph [flags] <config.yaml>

  -c, --config <file>   path to YAML config (positional arg also works)
  -i                    reuse an existing VM with the same name without prompting
  -k                    keep the VM after the task instead of deleting it
  -v, --verbose         debug logging
  --pretty              human-readable console logs instead of JSON
  --otel                enable OpenTelemetry logging; --otel=false disables (also MPH_OTEL, YAML otel.enabled)
  --version             print version and exit
  -h, --help            show help
```

Note: `-i` and `-k` have no long forms.

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | The agent reported `Completed`. |
| `1` | The agent reported `Failed`, the run ended without a `task_status` outcome, or `mph` itself failed. |
| `2` | The agent reported `Error`: an execution problem prevented completing or evaluating the task. |

## Observability

OTel is **off by default**. Enable with `--otel` / `otel.enabled: true`, pointing at an OTLP/gRPC collector (default `localhost:4317`):

```bash
make otel-up                       # Grafana+Loki+Prometheus stack
./bin/mph --otel ./testing/test.yaml
# Grafana: http://localhost:3000 (admin/admin)
```

See [Enable OpenTelemetry](docs/how-to/enable_otel.md).

## Development

```bash
go test ./...          # or: make test
go vet ./...           # or: make vet
golangci-lint run
make test-cover        # coverage profile -> coverage.out
```

## Docs

- [Tutorial: run your first task](docs/tutorial/run_an_agent_with_task.md)
- [How-to guides](docs/index.md): VM, LLM, commands, secrets, content, lifecycle, OTel
- [Reference](docs/reference/arch.md): architecture, [models](docs/reference/good_models.md)
- [Explanation](docs/explaination/tool_calling.md): [tool calling](docs/explaination/tool_calling.md), [meta commands](docs/explaination/meta_commands.md)
