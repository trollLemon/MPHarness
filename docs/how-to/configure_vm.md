# Configuring the VM

The `vm` section of your config controls the Multipass VM the agent runs commands in.

## Basic example

```yaml
vm:
  name: mph-vm
  disk: 20G
  ram: 4G
  cpu: 2
  image: noble
```

## Keys

| Key     | Required | Default     | Description |
|---------|----------|-------------|-------------|
| `name`  | no       | `mph-vm`    | Multipass instance name. Defaults to `mph-vm` if omitted.|
| `disk`  | yes      | none       | Disk size, e.g. `20G`. |
| `ram`   | yes      | none       | Memory, e.g. `4G`. |
| `cpu`   | yes      | none       | CPU count, must be greater than `0`. |
| `image` | no       | default LTS | Ubuntu image to use (see below). |

Top-level `content_dir` copies a local directory into the VM before the agent starts (see [Provide local content](provide_content.md)):

```yaml
vm:
  disk: 20G
  ram: 4G
  cpu: 2
model: unsloth/Qwen3-0.6B-Q8_0
prompt: "test the build in /home/ubuntu/content"
content_dir: ./my-build
```

The config fails validation if `disk`, `ram`, or `cpu` is missing or invalid, so always fill them in.

## Choosing an image

`image` accepts a Multipass image argument in several forms:

| Form | Example | Meaning |
|------|---------|---------|
| Ubuntu series | `noble` | Ubuntu LTS series name. |
| Version | `22.04` | Ubuntu version number. |
| Release stream | `release:noble` | Explicit release channel. |
| Daily stream | `daily:resolute` | Rolling daily builds of a series. |

Known series include `noble`, `jammy`, `focal`, `bionic`, and `resolute`. Run `multipass find` to list what your Multipass installation offers.

If `image` is omitted, Multipass's default LTS image is used.

## Related

- [Restrict what the agent may run in the VM](restrict_commands.md)
- [Manage the VM lifecycle](manage_vm_lifecycle.md)
- [Software architecture](../reference/arch.md)
