# Grounding

Input: the need, the design, and a read-only checkout of each target
project at its recorded commit.

For every target project, verify in the code what the design assumes:

- the files, functions, types and patterns the design names, with the path
  and the symbol as they exist;
- how the project builds and tests: the verify command the project declares,
  and the instruction file (AGENTS.md or CLAUDE.md) at the root;
- anything the design assumes that the code contradicts.

Use read, grep and glob on the checkout and repo_info for the project facts.
Every claim carries the source tag of the tool result it came from.

Produce markdown with one `## <project>` section per project, each with
`### Verified`, `### Contradictions` and `### Verify command`. Submit with a
data object `{"projects": {"<project>": {"sha": "<sha>", "verify_command": "<cmd>", "instruction_file": "<AGENTS.md|CLAUDE.md|none>"}}}`.
