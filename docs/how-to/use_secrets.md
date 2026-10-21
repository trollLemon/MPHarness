# Passing secrets to the agent

Tasks like "attach this Pro account and run the healthcheck" need a secret, but the prompt, the tool calls, and the logs are all visible to the model and to Loki, leaving nowhere safe to write one. Placeholders solve this: you write `%{PRO_TOKEN}` in your prompt, the agent copies it into its `vm_exec` calls, and `mph` swaps in the real value at the last moment before the command reaches the VM. Neither the model nor the log ever sees the value.

```yaml
prompt: "run sudo pro attach %{PRO_TOKEN} and report the status"
```

## Choosing a source

```yaml
secrets:
  source: env        # env (default) | keyring
  service: mph       # keyring service name; default "mph"
```

| `source` | Backing | Lookup |
|---|---|---|
| `env` (default) | host process environment | `os.LookupEnv(name)` |
| `keyring` | freedesktop Secret Service (GNOME Keyring, KWallet) | entry under `service` with the marker name as username |

Zero config is the common case: with `source` defaulting to `env`, `PRO_TOKEN=xyz ./bin/mph config.yaml` works with no `secrets:` block at all. An empty or unset variable counts as absent, so a CI job that forgot to export the variable fails fast instead of substituting nothing.

For desktop use, store the entry with your own tooling (there is no `mph` subcommand for this):

```bash
printf '%s' "$PRO_TOKEN" | secret-tool store --label='mph: PRO_TOKEN' service mph username PRO_TOKEN
```

## Marker syntax

A placeholder is `%{NAME}` where `NAME` matches `[A-Za-z_][A-Za-z0-9_]*`:

```
run sudo pro attach %{PRO_TOKEN}
curl -H "Authorization: Bearer %{GH_PAT}" https://api.github.com/user
```

A marker only matches when bounded: `foo%{BAR}` and `%{BAR}baz` are left alone, because the placeholder is glued to a word and the delimiters are doing more than delimiting.

## What happens on failure

Before the VM is created, `mph` checks every marker in your prompt against the configured source, so a missing `PRO_TOKEN` fails in seconds instead of fifteen minutes into a run. The prompt itself is never substituted: that would put the secret into the model's context.

Markers the model invents later are resolved per command. An unresolvable marker returns an ordinary tool error the model sees and can recover from, exactly like any other tool failure.

## Value safety

Substitution is raw text inserted into a string that becomes `bash -c`, so a value containing a space, quote, `$`, or backtick would be reinterpreted by the remote shell. Values must therefore match `[A-Za-z0-9._+/=:@-]+` (non-empty), which covers GitHub PATs, JWTs, base64, UUIDs, and AWS/Azure key shapes. Anything else is rejected with `unsafe secret value` before any VM round trip: at startup when the marker is in your prompt, per command otherwise. The error never contains the value.

## Output redaction

Once a secret has been used in a run, it can never come back out through VM output. Every value resolved so far is scrubbed from the output of every later command, with the placeholder put back in its place:

```
attach %{PRO_TOKEN}     # agent's command
pro attach: bad token %{PRO_TOKEN}   # what the model sees back
```

This matters most for captured output: `capture: true` redirects stdout to a raw file in the VM, and a later `output_read` carries no markers at all, yet a secret sitting in that file is still masked before the model sees it. The trade is deliberate: the agent cannot read back a secret it printed.

## Scope and limitations

- Substitution happens only inside commands run in the VM (`vm_exec` and the harness's own VM commands). `prompt`, `model`, `content_dir`, `otel.*`, `allowed_commands`, and the VM resource fields (`vm.name`, `vm.disk`, `vm.ram`, `vm.cpu`, `vm.image`) are never substituted. (`vm.kernel` is the exception: the harness installs it via a command run in the VM, so a marker there would be substituted.)
- One source per run; there is no per-marker override.
- The VM-side raw capture file still holds the secret on disk. Redaction stops it coming back through `mph`, not `mph` un-writing it: any command the agent runs in the VM (`cat`, `env`) can still read what was printed there.

## Related

- [How tool calling works](../explaination/tool_calling.md)
- [Restrict commands the agent may run](restrict_commands.md)
- [Software architecture](../reference/arch.md)
