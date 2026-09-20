# Meta commands

In `allowed_commands`, entries are literal command names such as `apt` or `git`. A meta command is a shorthand that expands to a list of commands at config-load time.

## `coreUtils`

Currently, the only meta command is `coreUtils`. Instead of writing standard utilities manually, you can instead write:

```yaml
allowed_commands:
  - coreUtils
  - apt
  - python3
```

`coreUtils` expands to the GNU core utilities. 

### File utilities

`chgrp`, `chmod`, `chown`, `cp`, `dd`, `df`, `dir`, `dircolors`, `du`, `install`, `ln`, `ls`, `mkdir`, `mkfifo`, `mknod`, `mktemp`, `mv`, `rm`, `rmdir`, `shred`, `sync`, `touch`, `vdir`

### Text utilities

`base32`, `base64`, `cat`, `cksum`, `comm`, `csplit`, `cut`, `expand`, `fmt`, `fold`, `head`, `join`, `md5sum`, `nl`, `od`, `paste`, `ptx`, `pr`, `sha1sum`, `sha224sum`, `sha256sum`, `sha384sum`, `sha512sum`, `shuf`, `sort`, `split`, `sum`, `tac`, `tail`, `tr`, `tsort`, `unexpand`, `uniq`, `wc`

### Shell and system utilities

`[`, `arch`, `basename`, `chcon`, `date`, `dirname`, `echo`, `env`, `expr`, `factor`, `false`, `groups`, `hostid`, `id`, `link`, `logname`, `nice`, `nohup`, `nproc`, `numfmt`, `pathchk`, `pinky`, `printenv`, `printf`, `pwd`, `readlink`, `realpath`, `runcon`, `seq`, `sleep`, `stat`, `stty`, `tee`, `test`, `timeout`, `true`, `tty`, `uname`, `unlink`, `uptime`, `users`, `who`, `whoami`, `yes`

## Why they exist
Listing `ls`, `cat`, `sort`, `head`, `grep` manually is noise and easy to get wrong (and forget something). Meta commands encode "the usual core toolkit" in one config option which makes the config file easier to maintain.

The only cases you wouldn't need this is if you know the exact few commands the agent needs to run, i.e a small set of steps, or running one piece of software in the VM.

## Caveats

- `coreUtils` does **not** include non-core utilities. `apt`, `pip`, `git`, `curl`, `python3`, `sudo` and friends must still be listed explicitly.
- It does not expand nested inside a command string: `"coreUtils"` as a bare entry works; something like `"coreutils ls"` is treated as a literal command name and will not produce any meaningful output.
- Trailing whitespace and duplicates are trimmed/deduplicated automatically, so mixing `coreUtils` with an explicit `ls` is fine.

## Related

- [Restricting commands (how-to)](../how-to/restrict_commands.md)
- [Software architecture](../reference/arch.md)
