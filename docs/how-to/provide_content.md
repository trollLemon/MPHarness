# Providing local content to the VM

Use `content_dir` when you want the agent to test locally-built artifacts without publishing them.

## Config

```yaml
vm:
  disk: 20G
  ram: 4G
  cpu: 2
model: unsloth/Qwen3-0.6B-Q8_0
prompt: "Build and test the project in /home/ubuntu/content"
content_dir: ./dist
```

- `content_dir` is optional. When omitted, no copy is performed and the system prompt is unchanged.
- When set, it must be an existing local directory (validated at `mph` startup). Relative paths are resolved against the working directory where `mph` is invoked.
- The harness creates `/home/ubuntu/content` in the VM (`mkdir -p`) and runs `multipass transfer --recursive --parents <content_dir> <vm>:/home/ubuntu/content` after the VM is ready and before the agent loop starts. It works for both fresh VMs and reused VMs (`-i`).

## What the agent sees

If `content_dir` is set, the system prompt gains an additional section:

> The user has provided a local directory whose contents have been copied to `/home/ubuntu/content` in the VM before execution. This content contains files needed to complete the task. Inspect it as needed (e.g. `ls -R /home/ubuntu/content`).

The top-level value is also recorded on the `mph.run` span (`mph.content_dir`) and a child `mph.vm.content` span covers the `mkdir` + `transfer` work. Check logs for `transferring content to VM` / `transfer complete`.

## Tips

- Keep the directory small. `transfer` copies recursively.
- The destination is always `/home/ubuntu/content`. Reference that absolute path in your `prompt`.
- Combine with `allowed_commands` to let the agent run unpack / build tools inside the VM:

```yaml
content_dir: ./my-build
allowed_commands:
  - coreUtils
  - tar
  - make
```

## Related

- [Configure the VM](configure_vm.md)
- [Restrict commands](restrict_commands.md)
- [Software architecture](../reference/arch.md)
