# Restricting the commands the agent may run

By default, `mph` lets the agent run **any** command inside the VM through `multipass_exec`. If you want the agent sandboxed to a fixed toolset, use `allowed_commands`.

```yaml
allowed_commands:
  - apt
  - apt-get
  - python3
  - pip
  - git
  - ls
  - cat
```

When the list is present, every command the agent tries to run is parsed and checked through an abstract syntax tree: any command that is not allowed is rejected with an error the agent sees and must report. When the list is empty or omitted, everything is allowed.

The agent is made aware of the allowed commands via the system prompts, so the agent generally will only call allowed commands, unless one is missing that is commonly used or required to complete the task. If the agent cannot complete the task due to a missing tool, it will abort.

## How matching works

An entry names a **command** (binary name). All command flags are assumed to be valid and are therefore not restricted by the config. 

```yaml
allowed_commands:
  - git      # allows: git clone ..., git status, git commit -m ...
  - apt      # allows: apt update, apt install -y ...
```

Matching is case-sensitive and matches against the first token of each executed command.

## Combinations inside a command

Commands are parsed as real shell, so operators and composition are handled correctly; each executed command is checked individually:

```yaml
allowed_commands:
  - apt
  - ps
  - grep
```

| Command run by the agent | Result |
|--------------------------|--------|
| `apt update && apt install -y curl` | Allowed (both are `apt`). |
| `apt update \|\| echo failed` | Rejected (`echo` not allowed). |
| `ps aux \| grep nginx` | Allowed (`ps` and `grep`). |
| `ls; cat /etc/passwd` | Allowed if both `ls` and `cat` are listed. |
| `ls $(rm -rf /)` | Rejected (the hidden `rm` is found inside `$()`). |

Because this is grammar-aware, things like command substitution (`$(...)`), backticks, pipes, and process substitution are all inspected. The check fails **closed**: a command whose executable name cannot be statically resolved (for example `$EDITOR` or `\ls`) is rejected.

## Special cases

- `true` and `false` are always allowed and ignored by the check (they are noise tokens; `ls \; true` is fine).
- The allowed list does not grant the agent `sudo` or `env` etc. unless you list those binaries too. To let the agent run `sudo apt update`, you need both `sudo` and `apt` in the list.
- Prefix-only caveat: `apt` in the list does **not** allow `apt-get`. List `apt-get` explicitly if you want it.
- Untracked non-literal names fail closed (see above).

## The `coreUtils` meta command

Listing every core utility by hand is tedious, so the harness expands one meta entry for you:

```yaml
allowed_commands:
  - coreUtils
  - apt
```

`coreUtils` expands to the standard GNU core utilities (file, text, and shell/system utilities, from `ls`, `cat` and `sort` to `date`, `sleep` and `wc`). See [Meta commands](../explaination/meta_commands.md) for the full list.

## Related

- [Meta commands (what `coreUtils` actually is)](../explaination/meta_commands.md)
- [How tool calling works](../explaination/tool_calling.md)
- [Software architecture](../reference/arch.md)
