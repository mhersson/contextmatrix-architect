# AGENTS.md - ContextMatrix Architect

## What is this project?

contextmatrix-architect turns a stated need into a blueprint: research,
decisions and a set of ContextMatrix (CM) cards written so an autonomous
contextmatrix-agent run completes each one first time. It is a third CM
backend speaking the chat-backend protocol: CM's chat panel is its only
client. Read `docs/design.md` before changing anything; it is the
specification and must stay true at HEAD.

## Boundaries

- Never runs cards, never writes code. Reads code through disposable
  read-only checkouts.
- Holds no secret but the backend HMAC key. The LLM endpoint, MCP key,
  git-credentials token and search key arrive in every session start from CM
  and live in memory only. There is no CM-less mode.
- Every action is logged: full transcripts (less web search result lists and artifact reads) and an append-only event log.
  The append-only tables (events, transcripts, artifacts, gates, emissions, questions) are never updated
  or deleted.

## Package map

- `cmd/contextmatrix-architect` - entrypoint; `serve` is the only server command.
- `internal/cli` - cobra commands: serve, config defaults, config validate.
- `internal/config` - koanf config: file, then `CONTEXTMATRIX_ARCHITECT_*` env.
- `internal/webhook` - CM-facing chat-backend protocol on backendkit webhookcore and logbridge; routes the abort, export, gate, cap, resume and continue commands and round replies that `internal/blueprint` recognises; serves the blueprint view, artifacts and live stream.
- `internal/export` - renders a blueprint's stored record as a markdown bundle in the state directory.
- `internal/blueprint` - controller: blueprint state machine, the step list that repeats per-slice stages once per slice, the digests of other slices' artifacts and `read_artifact`, stage chaining, responder policies and the gate reply, the auto responder, gates at any stage and their folding, reviewer rounds and their findings ledger, code stage preparation, the readiness loop, reply handling (answers, the need, notes delivered to later runs), abort and export, question rounds (posting, replies, continuation, chat), assumptions, events, sign-off, emission and the run watch, the blueprint view and the runs in flight, the live stream of views and drafts, the research tools for research stages and reviewers, their ledger and the guard's terms, the plain stops for a run its context window refused or stopped, and the research-off notice.
- `internal/stage` - one harness run: persona + instructions + the round tools (ask_questions, answer_chat) and submit_artifact, plus per-run tool sets (the code tools and `repo_info`), submit validation and a check's refusal for good, which fails the run (`fatal_check`), the pre-flight size check against the model's window, the final-reply mark on the reply a run ends on, the draft of the artifact body as it streams, and transient tool results kept out of the record.
- `internal/pack` - pipeline packs: stages, personas, instructions, lists.yaml and card.md; the default pack is embedded.
- `internal/code` - read-only mirrors and per-blueprint trees for the code stages; repo facts; the names a checkout declares, for the leak guard.
- `internal/research` - web research: the `web_search` and `web_fetch` tools, the leak guard that refuses code-shaped and project-specific text in search queries and URLs, the fetcher that reads public pages only, as readable text, and the search provider (Brave).
- `internal/cm` - MCP client to ContextMatrix: list_projects, the read-only `cm_` tools stage runs get, and typed card, playbook and run calls.
- `internal/contract` - the card contract: lists data and the mechanical checks.
- `internal/store` - SQLite: blueprints, stage runs, artifacts, transcripts, events, sources, gates, emissions, questions.

## Coding conventions

### Go

- Code under `internal/`; interfaces live in the package that uses them.
- Wrap errors with `fmt.Errorf("operation: %w", err)`. Never swallow.
- `context.Context` is the first parameter of any function doing I/O.
- No global state, no `init()`; dependencies are struct fields wired in `internal/cli/serve.go`.
- `log/slog` with structured fields. Never log a payload that carries a secret.
- Tests next to code, table-driven, `t.TempDir()`, `t.Helper()`; testify `assert` for checks, `require` for fatal ones. Tests exercise behaviour, never prose.
- Pack prose (personas, stage instructions) is never asserted on. The only permitted check is that a pack parses and every reference resolves.
- `gofumpt` formatting; `golangci-lint run` is read-only.

### Documentation

- Every statement matches the code at HEAD; a change that makes a doc line false updates it in the same commit.
- Comments explain non-obvious decisions and invariants only. No em-dashes; use hyphens.
- Never reference plan phases, task numbers or private card IDs.
- Never put organisation-specific hosts, keys or IDs in repo files, and never name an LLM gateway operator.

## Running and verifying

```bash
make build      # binary at ./contextmatrix-architect
make test
make test-race  # CGO_ENABLED=1
make lint
make fmt
contextmatrix-architect serve --config config.yaml
./svc.sh verify # check the generated user unit
```

Grep in stage runs execs ripgrep and glob also takes fd; install `rg` locally for manual runs.

Before any commit: `make fmt`, `make test`, `make lint`, `make build` clean.

## Commit discipline

- Never commit to `main`; work on a feature branch. Never push without explicit approval.
- Conventional commits: `type(scope): summary`, bullet-point body.
- No card IDs, plan references or session links in commits or PRs.
