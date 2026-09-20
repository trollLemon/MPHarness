# MPHarness

MPHarness is a local harness for running agent-driven tasks inside a VM. You describe a task, an LLM, and a VM, and `mph` orchestrates the process, it downloads the model, 
creates the VM, lets an LLM drive a tool-calling loop to complete the task, and hands the results back to you.

- **[Tutorials](tutorial/run_an_agent_with_task.md)**: learn by doing your first run end to end.
- **[How-to guides](how-to/)**: step-by-step guides for specific tasks:
  - [Configure the VM](how-to/configure_vm.md)
  - [Configure the LLM](how-to/configure_llm.md)
  - [Restrict commands the agent may run](how-to/restrict_commands.md)
  - [Manage the VM lifecycle](how-to/manage_vm_lifecycle.md)
- **[Reference](reference/)**: precise technical descriptions:
  - [Software architecture](reference/arch.md)
  - [Good models](reference/good_models.md)
- **[Explanation](explaination/)**: background and concepts:
  - [How tool calling works](explaination/tool_calling.md)
  - [Meta commands](explaination/meta_commands.md)
