# Grounding

Input: the need, the shared decisions, the design of the slice the task
names, the earlier slices' design, acceptance and cards as digests (`read_artifact`
returns the whole), and a read-only
checkout of each target project at its recorded commit. When the task names
no slice, verify the design of every slice.

For every target project the design touches, verify in the code what it
assumes:

- the files, functions, types and patterns the design names, with the path, the
  line and the symbol as they exist;
- how the project builds and tests: the verify command the project declares
  in ContextMatrix (repo_info), and when it declares none, the commands the
  repository documents (README, Makefile, CI); and which of its test suites
  a worker can run;
- the instruction file (AGENTS.md or CLAUDE.md) at the root;
- anything the design assumes that the code contradicts.

The earlier slices' cards are not built yet. What they will add is not in
the code, so a design that relies on it is no contradiction; name the
earlier card that delivers it instead.

A missing verify command, a missing instruction file or an empty repository
(repo_info marks it empty) is not a contradiction: decomposition plans
that setup as work. Record it under `### Verify command`. A command only
the shared decisions, the design or an earlier slice's card plans is not
one the repository documents: name it there as planned, and keep it out of
`verify_candidate`. In an empty repository, check a declared verify command
only against the services a worker lacks: the scripts and targets it names
do not exist yet.

A verify command runs in a worker: a container with the repository checkout,
its image's language toolchain and network access for packages, and no
database, no Docker daemon, no cluster and no deployed environment. The
agent runs the tests it writes there too, whatever the verify command is.

For every target project, declared verify command or not, read each test
suite's setup, down to what its tests load before they run. A suite whose
setup needs a database, Docker, containers the tests start, a cluster, a
deployed environment or another service a worker lacks is left to the pull
request's CI: record it under `### Verify command` with its evidence. A
service a CI job starts for a suite is evidence that the suite needs it.

Check every verify command against the worker:

- Every script, package script or build target the command names exists
  where the command runs; cite where it is defined `[S<n> path:line]`. A
  command that runs a package script at the repository root needs that
  script in the root manifest.
- Read what each named script or target runs. A command that runs a suite
  left to the pull request's CI is not verify material.
- CI configuration is a source, never proof: CI provides services a worker
  lacks.

`verify_candidate` is what remains of what the repository documents, such
as its lint, its type checks and the unit tests that need no service, and
empty when nothing remains. A verify command the project declares in
ContextMatrix that fails these checks is a contradiction: report it under
`### Contradictions` with the evidence. It stays the project's verify
command, and `verify_candidate` stays empty.

Use read, grep and glob on the checkout and repo_info for the project facts.
Cite every claim `[S<n> path:line]`: the source tag of the tool result it came
from and the line grep or the read's range shows; `[S<n> path]` for a whole
file, `[S<n>]` for repo_info and the cm_ tools.

Use the cm_ tools to find open cards in the target projects that already
cover part of the need, and list them in each project's section under
`### Existing cards` with their ids and the ContextMatrix source tag.

The task lists the blueprint's assumptions with their ids. For each one the
code settles, add a verdict: verified or contradicted, with the source tag
and what you found; leave the rest out.

Produce markdown with one `## <project>` section per project, each with
`### Verified`, `### Contradictions` and `### Verify command`. Submit with a
data object `{"projects": {"<project>": {"sha": "<sha, empty for an empty repository>", "verify_command": "<declared in ContextMatrix, empty when none>", "verify_candidate": "<what the repository documents and a worker can run, when none is declared; empty when nothing remains>", "instruction_file": "<AGENTS.md|CLAUDE.md|none>"}}, "verdicts": [{"id": "A1", "status": "verified|contradicted", "evidence": "[S1] <what you found>"}]}`.
