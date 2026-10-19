# Configuring the VM

The `vm` section of your config controls the LXD VM the agent runs commands in.

## Basic example

```yaml
vm:
  name: mph-vm
  disk: 20G
  ram: 4G
  cpu: 2
  image: resolute
  kernel: linux-image-7.0.0-14-generic
```

## Keys

| Key     | Required | Default     | Description |
|---------|----------|-------------|-------------|
| `name`  | no       | `mph-vm`    | LXD instance name. Must follow DNS hostname rules (no underscores). Defaults to `mph-vm` if omitted. |
| `disk`  | yes      | none       | Disk size, e.g. `20G`. |
| `ram`   | yes      | none       | Memory, e.g. `4G`. |
| `cpu`   | yes      | none       | CPU count, must be greater than `0`. |
| `image` | no       | `ubuntu:24.04` | Ubuntu image to use (see below). |
| `kernel` | no       | none        | APT kernel package to install (e.g. `linux-image-7.0.0-14-generic` or `linux-image-unsigned-7.0.0-14-realtime`). Installs kernel, modules, and modules-extra, updates grub, reboots, and verifies the new kernel is running before handing off to the agent. |

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

`image` accepts Ubuntu image aliases. Bare values resolve against the `ubuntu` remote:

| Form | Example | Remote | Meaning |
|------|---------|--------|---------|
| Empty | none | `ubuntu` | `24.04`, the default. |
| Ubuntu series | `noble` | `ubuntu` | Ubuntu series name. |
| Version | `22.04` | `ubuntu` | Ubuntu version number. |
| Release stream | `release:noble` | `ubuntu` | Explicit release channel. |
| Daily stream | `daily:resolute` | `ubuntu-daily` | Rolling daily builds of a series. |
| Ubuntu remote | `ubuntu:noble` | `ubuntu` | Alias requested explicitly from the `ubuntu` remote. |
| Ubuntu daily remote | `ubuntu-daily:resolute` | `ubuntu-daily` | Explicit daily channel. |
| Images remote | `images:ubuntu/24.04` | `images` | Alias from the `images` remote. |
| Any other `remote:alias` | `custom:noble` | n/a | Rejected with an error naming the supported prefixes. |

Known series include `noble`, `jammy`, `focal`, `bionic`, and `resolute`. Run `lxc image list ubuntu:` or `lxc image list ubuntu-daily:` to list aliases from those remotes.

VM images must include the LXD agent, which mph uses to run commands and copy files. Official Ubuntu images for 20.04 and newer include it; the 18.04 images require a manual agent installation. Without the agent, VM creation succeeds but readiness times out and the run fails.

If `image` is omitted, `ubuntu:24.04` is used. An unknown alias, or an alias that only exists as a container image, fails before any VM is created.

## Guest user

Commands run as the image's `ubuntu` user (UID/GID 1000); use `sudo` when root privileges are needed.

After the VM is ready, mph waits for cloud-init to finish. If the `ubuntu` user is missing or does not have passwordless sudo, mph creates the user with a home directory and grants passwordless sudo, logging the setup step.

## Related

- [Restrict what the agent may run in the VM](restrict_commands.md)
- [Manage the VM lifecycle](manage_vm_lifecycle.md)
- [Software architecture](../reference/arch.md)
