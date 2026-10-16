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
- Holds no secret but the backend HMAC key. The LLM endpoint, MCP key and
  git-credentials token arrive in every session start from CM and live in
  memory only. There is no CM-less mode.
- Every action is logged: full transcripts and an append-only event log.
  The append-only tables (events, transcripts, artifacts) are never updated
  or deleted.

## Package map

- `cmd/architect` - entrypoint; `serve` is the only server command.
- `internal/cli` - cobra commands: serve, config defaults, config validate.
- `internal/config` - koanf config: file, then `CONTEXTMATRIX_ARCHITECT_*` env.
- `internal/webhook` - CM-facing chat-backend protocol on backendkit webhookcore and logbridge; reply parsing.
- `internal/blueprint` - controller: blueprint state machine, questions, events.
- `internal/stage` - one harness run: persona + instructions + tools + output schema.
- `internal/pack` - pipeline packs: stages, personas, instructions; the default pack is embedded.
- `internal/store` - SQLite: blueprints, stage runs, artifacts, transcripts, events.

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
make build      # binary at ./architect
make test
make test-race  # CGO_ENABLED=1
make lint
make fmt
architect serve --config config.yaml
```

Before any commit: `make fmt`, `make test`, `make lint`, `make build` clean.

## Commit discipline

- Never commit to `main`; work on a feature branch. Never push without explicit approval.
- Conventional commits: `type(scope): summary`, bullet-point body.
- No card IDs, plan references or session links in commits or PRs.
