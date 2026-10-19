# Running your first agent-driven task

In this tutorial you will run `mph` for the first time. By the end you will have a small LLM run a couple of commands inside an LXD VM that the harness brings up for you, all from a single YAML file.

## What you'll learn

- What a minimal `mph` config looks like.
- How to run the `mph` binary.
- What happens in the background while it runs.
- How to read the final output.

## Prerequisites

- [LXD](https://linuxcontainers.org/lxd/) 5.x LTS or newer installed (deb or snap) with a running daemon.
- Access to the LXD socket, typically by being in the `lxd` group.
- A working default profile from `lxd init` (a root disk on a storage pool, plus a network).
- Enough free disk space (the VM image, the model files, and the llama.cpp runtimes need a few gigabytes).
- Hardware that can run a small LLM. A CPU-only machine works for a 0.6B to 1.7B model; a GPU with a few GB of VRAM makes a bigger model practical.

## Step 1: Write a config file

Create a file called `first-task.yaml`:

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
  3. Summarise the test results.
```

Two pairs of settings matter most here:

- `vm` describes the VM the harness will bring up for the agent. The name must follow DNS hostname rules, so `mph-tutorial` is valid and underscores are not.
- `model` names the LLM `mph` will download and run locally.
- `prompt` is the task you are giving the agent.

Everything else is optional; the values above are sensible defaults for a CPU-only first run.

## Step 2: Run the application

```bash
./bin/mph first-task.yaml
```

The config file can be passed as a positional argument or with `-c`/`--config`:

```bash
./bin/mph -c first-task.yaml
```

Add `-v` if you want debug-level logging, and `--pretty` for human-readable (instead of JSON) logs:

```bash
./bin/mph -v --pretty first-task.yaml
```

Additional flags:

- `-c`, `--config <file>`: path to YAML config file (positional arg also works)
- `-i`: reuse an existing VM with the same name without prompting
- `-k`: keep the VM after the task completes instead of deleting it
- `-v`, `--verbose`: verbose (debug) logging
- `--pretty`: pretty-print logs (human-readable console) instead of JSON
- `--version`: print version and exit
- `-h`, `--help`: show help

```bash
./bin/mph -i -k first-task.yaml
```

On the first run, `mph` must download the llama.cpp native libraries and the model. This can take a while, but all downloads are cached so subsequent runs wont need to download the required libraries again.
## Step 3: Follow what happens

You'll see log lines for each stage:

```text
loaded config                                  # your YAML parsed and validated
Starting inference                             # the agent loop begins
model output                  part=reasoning   # the model's chain of thought
agent tool call   tool=vm_info                # "what's the VM state?"
tool succeeded    tool=vm_exec                # each command returns its output
token usage                                    # one log line per LLM response
```

The sequence you observe is the agent loop: the LLM reads the task, decides on one tool call, runs it, reads the tool result, and repeats until it is done. The VM is already running (the harness brought it up), so the agent starts with a `vm_info` call to orient itself, then runs a series of `vm_exec` commands until the task is complete.

## Step 4: Read the final output

When the agent stops, the harness prints the output of every command it ran (separated by `---` markers). The model's final summary appears only in the logs (as a `model output` info line), not in the console output.

The VM's lifecycle is managed by `mph`, outside the agent loop. You can check its state with:

```bash
lxc list
```

## What could go wrong?

- **`mph` cannot connect to LXD**: make sure the daemon is running and your user can access its socket (typically by being in the `lxd` group). If there is no default profile, run `lxd init`.
- **`config file required`**: pass the config file as an argument or with `-c first-task.yaml`.
- **Timeouts**: on slow hardware a single inference call can exceed `chat_timeout`. Raise it in the `agent` section (the example uses `30m` for exactly this reason).
- **The model download keeps failing**: check your network, then look up a different [model id](../reference/good_models.md).

## Next steps

- Tune the VM and LLM to your machine: [Configure the VM](../how-to/configure_vm.md), [Configure the LLM](../how-to/configure_llm.md).
- Test a locally-built artifact: [Provide local content](../how-to/provide_content.md).
- Keep the agent from running anything you don't want: [Restrict commands](../how-to/restrict_commands.md).
- Learn how the agent interacts with the VM: [How tool calling works](../explaination/tool_calling.md).
