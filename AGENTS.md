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
- Every action is logged: full transcripts (less web search result lists and artifact reads, and reasoning details that redaction would change, dropped whole) and an append-only event log.
  The append-only tables (events, transcripts, artifacts, gates, emissions, questions) are never updated
  or deleted.

## Package map

- `cmd/contextmatrix-architect` - entrypoint; `serve` is the only server command.
- `internal/cli` - cobra commands: serve, config defaults, config validate.
- `internal/buildinfo` - the build's version, commit and build time, stamped by ldflags, and the merge history embedded at build.
- `internal/config` - koanf config: file, then `CONTEXTMATRIX_ARCHITECT_*` env.
- `internal/webhook` - CM-facing chat-backend protocol on backendkit webhookcore and logbridge; routes the abort, export, gate, cap, mode, resume, continue, retrospective, lessons and retire commands and the round and mockup replies that `internal/blueprint` recognises; serves the blueprint view, artifacts and live stream.
- `internal/export` - renders a blueprint's stored record as a markdown bundle in the state directory.
- `internal/blueprint` - controller: blueprint state machine, the step list that repeats per-slice stages once per slice, the digests of other slices' artifacts and `read_artifact`, stage chaining, responder policies, the gate reply and the mode switch, which hands a waiting round to the auto responder, the auto responder, gates at any stage and their folding, reviewer rounds and their findings ledger, code stage preparation, the readiness loop, reply handling (answers, the need, notes delivered to later runs), abort and export, question rounds (posting, replies, continuation, chat), assumptions, events, sign-off, emission and the run watch, the blueprint view and the runs in flight, the live stream of views and drafts, the research tools for research stages and reviewers, their ledger and the guard's terms, the proof-of-concept targets a run that writes or judges cards reads and is told, the checks over outcomes, acceptance traces and sweeps, restated assumptions and readiness's findings across the blueprint, the sign-off's override reason, the ended view, the plain stops for a run its context window refused or stopped, plain language (the audience split, consequences, the plain summary and wording), the mockup step (the offer, revisions as rounds inside the architect, the person's replies, the auto responder's turns and the hand-over and hand-back, the close and the settle, the approved design in words for later stages, and the mockup block on every card of a slice that builds its screens), each blueprint's lists, merged with its lesson set's overlays, the lesson set's text in decomposition's and readiness's instructions, the retrospective (the pass over the emitted cards on the run slot, its findings message and stored lessons, the lesson round with its own lookup, its answers and each lesson's decision, the `lessons` and `retire` replies and the first-stop offer), and the research-off notice.
- `internal/retro` - the retrospective's pure parts: a card's run signals and the struggle rules, the findings a retro run submits and their checks, the run's task and the lesson questions.
- `internal/stage` - one harness run: persona + instructions + the round tools (ask_questions, answer_chat) and submit_artifact, `show_mockup` and the frame-contract check, plus per-run tool sets (the code tools and `repo_info`), submit validation and a check's refusal for good, which fails the run (`fatal_check`), the pre-flight size check against the model's window, the final-reply mark on the reply a run ends on, the draft of the artifact body as it streams, and transient tool results kept out of the record.
- `internal/pack` - pipeline packs: stages, personas, instructions (`plain.md` for plain-language blueprints, the mockup responder's instructions, `retrospective.md` for the retrospective), lists.yaml and card.md and their merge with lesson overlays, and the rules a pack's screen and mockup stages keep; the default pack is embedded.
- `internal/code` - read-only mirrors and per-blueprint trees for the code stages; repo facts; the names a checkout declares, for the leak guard.
- `internal/research` - web research: the `web_search` and `web_fetch` tools, the leak guard that refuses code-shaped and project-specific text in search queries and URLs, the fetcher that reads public pages only, as readable text, and the search provider (Brave).
- `internal/cm` - MCP client to ContextMatrix: list_projects, the read-only `cm_` tools stage runs get, and typed card, playbook and run calls.
- `internal/contract` - the card contract: lists data, the mechanical checks, the screens a card names and the mockup blocks the phrase scans skip.
- `internal/store` - SQLite: blueprints, stage runs, artifacts, transcripts, events, sources, gates, emissions, questions, lessons and each blueprint's lesson set.

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

- Every statement matches the code at HEAD, in `README.md`, `docs/guide.md`, `docs/reference.md`, `docs/installing.md` and `docs/design.md` alike; a change that makes a doc line false updates it in the same commit.
- A screenshot in `docs/images/` is replaced when the behaviour it shows changes, not for cosmetic drift.
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
