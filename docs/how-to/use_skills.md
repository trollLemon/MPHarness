# Adding skills to the agent

Use `--skills-dir` to give the agent extra reference material: instructions, conventions, or checked-in playbooks that help it complete the task.

## Usage

```bash
mph --skills-dir ./skills config.yaml
```

The skills directory is flag-only; there is no YAML key. When the flag is omitted, the system prompt is unchanged.

## Layout

Each skill is its own subdirectory containing a `SKILL.md` file:

```
skills/
  git-conventions/
    SKILL.md
  package-debugging/
    SKILL.md
```

- The skill name is the first H1 heading in `SKILL.md` (`# Git conventions`), falling back to the subdirectory name when there is no H1.
- Subdirectories without a `SKILL.md` are ignored.
- Scripts and other assets are not supported. Only the markdown text is used.

## What the agent sees

When `--skills-dir` is set, the system prompt gains a skills section that explains the following markdown is user-provided reference material to consult when relevant, followed by each `SKILL.md` in full.

Files are scanned for prompt-injection patterns at startup, the same way `content_dir` is. Hits are logged as warnings; they are not blocked.

> **Warning:** skill contents are injected into the system prompt in full and consume context window. Keep each `SKILL.md` short and focused. A large skills directory reduces the room available for the task, tool output, and conversation history, and may trigger context compaction early.

## Related

- [Providing local content to the VM](provide_content.md)
- [Configure the VM](configure_vm.md)
- [Software architecture](../reference/arch.md)
