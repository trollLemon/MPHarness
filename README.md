# MPHarness
Local Harness for running tests in a Multipass VM without the worry of an agent going bonkers.

## Quick config

```yaml
vm:
  disk: 20G
  ram: 4G
  cpu: 2
model: unsloth/Qwen3-0.6B-Q8_0
prompt: "test the build"
content_dir: ./my-build   # optional: copied to /home/ubuntu/content in the VM
allowed_commands:
  - coreUtils
```

See `docs/` for tutorials and reference, or run `mph --help`.
