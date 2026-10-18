# Grounding

Input: the need, the shared decisions, the design of the slice the task
names, the earlier slices' design, acceptance and cards as digests (`read_artifact`
returns the whole), and a read-only
checkout of each target project at its recorded commit. When the task names
no slice, verify the design of every slice.

For every target project the design touches, verify in the code what it
assumes:

- the files, functions, types and patterns the design names, with the path
  and the symbol as they exist;
- how the project builds and tests: the verify command the project declares
  in ContextMatrix (repo_info), and when it declares none, the commands the
  repository documents (README, Makefile, CI);
- the instruction file (AGENTS.md or CLAUDE.md) at the root;
- anything the design assumes that the code contradicts.

The earlier slices' cards are not built yet. What they will add is not in
the code, so a design that relies on it is no contradiction; name the
earlier card that delivers it instead.

A missing verify command, a missing instruction file or an empty repository
(repo_info marks it empty) is not a contradiction: decomposition plans that
setup as work. Record it under `### Verify command`.

Use read, grep and glob on the checkout and repo_info for the project facts.
Every claim carries the source tag of the tool result it came from.

Use the cm_ tools to find open cards in the target projects that already
cover part of the need, and list them in each project's section under
`### Existing cards` with their ids and the ContextMatrix source tag.

The task lists the blueprint's assumptions with their ids. For each one the
code settles, add a verdict: verified or contradicted, with the source tag
and what you found; leave the rest out.

Produce markdown with one `## <project>` section per project, each with
`### Verified`, `### Contradictions` and `### Verify command`. Submit with a
data object `{"projects": {"<project>": {"sha": "<sha, empty for an empty repository>", "verify_command": "<declared in ContextMatrix, empty when none>", "verify_candidate": "<what the repository documents, when none is declared>", "instruction_file": "<AGENTS.md|CLAUDE.md|none>"}}, "verdicts": [{"id": "A1", "status": "verified|contradicted", "evidence": "[S1] <what you found>"}]}`.
