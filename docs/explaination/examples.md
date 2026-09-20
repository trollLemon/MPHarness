# Configuration Examples

This page collects complete, runnable YAML configs for common use cases. Each example is a standalone file you can save and run with `mph <file.yaml>`.

---

## Minimal

The smallest viable config. Uses defaults for everything optional.

```yaml
vm:
  disk: 20G
  ram: 4G
  cpu: 2
  image: noble
model: unsloth/Qwen3-0.6B-Q8_0
prompt: "Inside the VM, run `uname -a` and `ls /etc`, then summarize."
```

---

## Deterministic

Fixed sampling for reproducible runs (temperature 0.0, tight top_p/top_k). Useful for debugging or regression testing.

```yaml
llm:
  temperature: 0.0
  top_p: 0.1
  top_k: 1
  tool_choice: auto
  context_window: 4096
model: unsloth/Qwen3-0.6B-Q8_0
vm:
  disk: 20G
  ram: 4G
  cpu: 2
  name: mp-deterministic
prompt: "List files in /tmp"
```

---

## Creative

Higher temperature and top_p for exploratory tasks where you want varied output.

```yaml
llm:
  temperature: 0.8
  top_p: 0.95
  top_k: 100
  tool_choice: auto
  context_window: 0   # auto-tune
model: unsloth/Qwen3-0.6B-Q8_0
vm:
  disk: 20G
  ram: 4G
  cpu: 2
  name: mp-creative
prompt: "Explore the VM and propose three ways to optimize disk usage."
```

---

## Debug

Short timeouts and few iterations for quick iteration while developing prompts or configs.

```yaml
vm:
  name: debug-vm
  disk: 10G
  ram: 2G
  cpu: 2
  image: noble
model: unsloth/Qwen3-0.6B-Q8_0
prompt: "Run `echo hello` in the VM and return its output."
agent:
  max_iterations: 3
  chat_timeout: 60s
  total_timeout: 5m
```

---

## Sandbox

Restricted command set using an older Ubuntu base (bionic). Only `ls` and `cat` are permitted; any other command is rejected.

```yaml
vm:
  name: sandbox
  disk: 10G
  ram: 2G
  cpu: 1
  image: bionic
model: unsloth/Qwen3-0.6B-Q8_0
allowed_commands:
  - ls
  - cat
prompt: |
  In the VM, list /etc and cat /etc/os-release. Do not run anything else.
```

---

## Full-Featured

Shows every available option: allowed commands, tuned LLM, generous timeouts, and a multi-step prompt. Uses a larger model (27B) suited for GPU machines.

```yaml
vm:
  name: big-model
  disk: 20G
  ram: 4G
  cpu: 2
  image: noble 

model: unsloth/Qwen3.8-27B-UD-Q8_K_XL 
allowed_commands:
  - apt
  - apt-get
  - python3
  - pip
  - pip3
  - git
  - sudo
  - coreUtils

llm:
  temperature: 0.2
  top_p: 0.95
  top_k: 40
  tool_choice: auto   # auto | required | none
  context_window: 169216  # 0 = auto-tune

agent:
  max_iterations: 20
  chat_timeout: 30m # if running on CPU you may need to set this quite high
  total_timeout: 30m

prompt: |
  Inside the Multipass VM, do the following:
  1. Update apt and install python3, pip, git (this requires sudo)
  2. run ls /etc
  3. Summarise the test results.
  Create the VM if needed, when finished, remove the vm.
```

---

## Related

- [Configuring the VM](../how-to/configure_vm.md)
- [Configuring the LLM](../how-to/configure_llm.md)
- [Restricting commands](../how-to/restrict_commands.md)
- [Managing the VM lifecycle](../how-to/manage_vm_lifecycle.md)
