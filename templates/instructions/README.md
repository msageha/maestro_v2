# Instruction Templates

These Markdown files are the **source-of-truth** for agent instruction prompts.

## Sync with `.maestro/instructions/`

When `maestro setup <dir>` is run, these templates are copied into
`.maestro/instructions/` inside the target project directory.

**There is no automatic sync mechanism after initial setup.**
If you modify a template here, projects that were already set up will not
pick up the change until `maestro setup` is re-run for that project.
