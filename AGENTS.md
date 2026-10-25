# AGENTS.md - ContextMatrix Architect

## What is this project?

contextmatrix-architect turns a stated need into a blueprint: research,
decisions and a set of ContextMatrix (CM) cards written so an autonomous
contextmatrix-agent run completes each one first time. It is a third CM
backend speaking the chat-backend protocol: CM's chat panel is its only
client. Read `docs/decisions.md` before changing anything: it records the
decisions the code keeps and why, and must stay true at HEAD.

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
- `internal/cli` - cobra commands; serve's wiring, the panel rendering of controller events, the workspace sweeper and the search-tool check.
- `internal/buildinfo` - the build's version, commit and build time, stamped by ldflags, and the merge history embedded at build.
- `internal/config` - koanf config: file, then `CONTEXTMATRIX_ARCHITECT_*` env.
- `internal/webhook` - CM-facing chat-backend protocol on backendkit webhookcore and logbridge; routes the abort, export, gate, cap, mode, resume, continue, retrospective, lessons and retire commands and the round and mockup replies that `internal/blueprint` recognises; serves the blueprint view, artifacts and live stream.
- `internal/export` - renders a blueprint's stored record as a markdown bundle in the state directory.
- `internal/blueprint` - the controller: the blueprint state machine and the pipeline of stage, reviewer and responder runs; question rounds, gates and their folding, assumptions and modes; code stage preparation and the readiness loop; the mockup step; sign-off, emission and the run watch; the retrospective and lessons; the blueprint view and the live stream.
- `internal/retro` - the retrospective's pure parts: a card's run signals and the struggle rules, the findings a retro run submits and their checks, the run's task and the lesson questions.
- `internal/stage` - one harness run: persona + instructions + the round tools (ask_questions, answer_chat) and submit_artifact, `show_mockup` and the frame-contract check, plus per-run tool sets (the code tools and `repo_info`), submit validation and a check's refusal for good, which fails the run (`fatal_check`), the pre-flight size check against the model's window, the final-reply mark on the reply a run ends on, the draft of the artifact body as it streams, and transient tool results kept out of the record.
- `internal/pack` - pipeline packs: stages, personas, instructions (`plain.md` for plain-language blueprints, `unattended.md` for the Unattended overlay, the mockup responder's instructions, `retrospective.md` for the retrospective), lists.yaml and card.md and their merge with lesson overlays, and the rules a pack's screen and mockup stages keep; the default pack is embedded.
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

- Documentation must always be true: every statement matches the code at
  HEAD, and a change that makes a doc line false updates it in the same
  commit.
- Document the current state - what exists now and why, not how it got here.
- Comments explain only non-obvious decisions, constraints, safety invariants,
  or workarounds. Do not narrate what code does or record change history; git
  holds history. Keep comments to one or two tight lines unless a longer
  explanation is genuinely necessary. Rewrite or delete if you find comments
  that don't follow this rule.
- Never use em-dashes; use hyphens (-).
- Never reference plan phases, task numbers, or private card IDs in doc comments.
- The docs held to this are `README.md`, `docs/guide.md`, `docs/reference.md`, `docs/installing.md` and `docs/decisions.md`.
- A screenshot in `docs/images/` is replaced when the behaviour it shows changes, not for cosmetic drift.
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

- **Never commit to `main`.** Work on a feature branch; tasks of a plan or
  scope the user approved commit there without asking again. Never push
  without explicit user approval.
- Conventional commits: `type(scope): summary`, bullet-point body.
- No card IDs or plan references in commits or PRs.
- No session links in commits or PRs.
