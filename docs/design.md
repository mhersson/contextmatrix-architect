# contextmatrix-architect - design

Status: agreed design, 2026-10-15. Slice 0 is implemented (`architect serve`, controller, store, intake stage) and slice 1 is under way; the rest is design.

## Purpose

The architect turns a stated need into a **blueprint**: a researched and
decided solution plus a set of ContextMatrix (CM) cards written so that an
autonomous `contextmatrix-agent` run completes each one on the first attempt.
The cards land in CM as a playbook, or as a single card when the need is that
small. Every stage before the cards exists only to make the cards better.

A user opens a blueprint session in CM's chat panel and states a need ("get
this project running containerised on Kubernetes"). The architect researches it,
plays the roles a team would (product owner, system designer, senior developer,
reviewer), asks what it cannot decide, records every decision and assumption,
drafts the cards, checks them against the card contract, and emits the
playbook. CM does what CM does from there. In yolo mode the whole path from
need to merged pull requests on a playbook branch runs without a human touching
it.

## Vocabulary

| Term          | Meaning                                                                                             |
| ------------- | --------------------------------------------------------------------------------------------------- |
| architect     | This tool.                                                                                          |
| blueprint     | The product for one need: artifacts, decisions, cards and playbook order. Lands in CM as a playbook, or one card. |
| stage         | One unit of AI work producing one artifact. Runs as one or more short harness runs.                 |
| gate          | A decision point after a stage: approve, or reject with feedback.                                   |
| question      | Something a stage cannot decide alone, raised through `ask_human`.                                  |
| responder     | Who answers a question or decides a gate: a human in the CM session, or the AI (`auto`).            |
| persona       | The role a run plays: product owner, system designer, senior developer, reviewer, responder.        |
| pack          | A team's stages, personas, gate policies, checks and card template. Data, not code.                 |
| source        | One external input a blueprint used, with its identity and version.                                 |
| card contract | The rules a card must satisfy to be emitted. The core of this design.                               |
| lesson        | A rule learned from how emitted cards fared, approved by a human, applied to later blueprints.      |

No stage is named "architecture"; that work is **solution design**, so
"architect" always means this tool.

## Boundaries

- The architect never runs cards and never writes code. It reads code through
  disposable read-only checkouts to ground cards in what exists.
- After emission CM is the source of truth for the cards. Corrections happen in
  CM.
- The architect's own store is the record. Nothing is written into a code repo
  unless a project opts into the markdown export.
- It is methodology-neutral. BDD, Gherkin, sizing schemes and compliance lists
  belong to a pack, never to the core. Given/When/Then in a card body is
  acceptance-criteria text; projects that want BDD execution opt in through a CM
  task skill.
- Jira is an optional adapter on both ends (seed a need from an issue, mirror a
  blueprint as an epic with tasks), never a state store. It is not part of v1.
  Confluence is not used.

## Decisions

Settled during design. Each has its reason so it is not re-litigated.

| Decision                                                  | Why                                                                                                                                                              |
| --------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Standalone Go service on `contextmatrix-harness`          | The stage controller, gates and cross-run ledger have to be written either way; the chat backend cannot host them (lossy resume, no terminal tools, no responder policy). CM stays a coordination layer rather than becoming a model runner. |
| Not built on `contextmatrix-agent`; `backendkit` only for the CM-facing side | The agent is shaped for card work in a container, and nothing under its `internal/` is importable. backendkit's `webhookcore` and `logbridge` are the chat-backend protocol the architect speaks towards CM, so they are used for exactly that and nothing else. |
| CM's chat panel is the only client                        | The architect implements CM's chat-backend protocol as a third backend kind, so CM's existing panel is where the need is stated, questions and gates are answered, and side operations (export, outcomes, lessons) are requested by reply. Same look and feel, session list, auth and browser for free. No UI, no CLI beyond `architect serve` and the `config defaults|validate` helpers, no client API. |
| One controller process                                    | `architect serve` alone owns the store, the workspaces and the harness runs, and every session originates in CM. One record, one place code is checked out, no laptop copy of anything. |
| Provisioned by CM, like the agent and chat backends      | Every blueprint is a CM session. The start payload delivers the LLM endpoint, the MCP key, the model and a git-credentials token. The architect's own config holds no secret but the backend HMAC key, and there is no CM-less mode. |
| Questions block inside the run; no persisted resume       | Nobody walks away from a blueprint for days. A blocking `ask_human` keeps one transcript and a warm prompt cache. A crash re-runs the open stage from stored artifacts. |
| SQLite is the record; markdown is a rendering             | One store for artifacts, gates, transcripts, audit and cost. Committing per-stage markdown into code repos bloats them fast; an opt-in automatic export serves teams that review in pull requests. |
| Everything is logged                                      | Every run's full transcript and an append-only event log. The append-only tables are never updated or deleted. The export is a complete audit record.           |
| A playbook for several cards, a bare card for one         | Manual entries are the only way to express human and live checks across cards. A one-card playbook would end in a merged PR on a playbook branch plus a second PR to main; a bare card ends in one PR to the default branch. The `emissions` table is the blueprint's handle in CM either way. |
| Fixed stage list, no composer; size is never estimated    | Every stage earns its place even for an infrastructure request; cheap stages collapse rather than skip. Gates fold and the playbook decision falls out of counts in the artifacts (slices, cards), so a need is never classified small or big up front. Evidence for skippable stages comes from Outcomes, which does not exist yet. |
| Personas attached to stages, no agent system              | Roles are prompt text in the pack. The composer-and-agents machinery of AI-DLC is a second decision engine the first one has not earned.                         |
| Jira out of v1, in as one later slice                     | The pipeline is exercised on the operator's own projects first. The adapter shape (seed in, mirror out, never state) keeps later integration small, provided sources carry kinds from day one. |
| Lessons are human-approved                                | A blueprint yields a handful of cards; early lessons are anecdotal. Human approval guards against learning noise.                                                 |
| No memory layer                                           | Live retrieval plus versioned sources. A knowledge layer, if ever, is fed only approved architect outputs.                                                        |
| No bespoke web UI                                         | CM's chat panel renders the architect's sessions. Richer controls (gate buttons, a gate inbox, a blueprint view) are CM web changes added when needed.           |

Rejected names: big-brain, Keymaker, Basis, Pivot, Factor, Span, Foreman,
orchestrator, researcher, explorer. "ContextMatrix" refers to ordering an LLM
context window; never use film references.

## Architecture

```
CM chat panel (the only client)
     |  internal/webhook (backendkit webhookcore + logbridge:
     |  session start, /message, /logs SSE, end-session)
     |  commands in: answer, approve, reject, abort, note, export, outcomes, lesson decision
     |  events out: stage progress, tool calls, questions, gates, cost
controller (internal/blueprint)     blueprint state machine, gates, responder policy, cost cap
     |
stage runner (internal/stage)       one harness.Run per round: persona + stage prompt +
     |                              pack rules + lessons + tool set + output schema
     |-- code tools      read-only shallow clone per project at a recorded sha;
     |                   harness read/grep/glob jailed to it; repo_info
     |-- research tools  web_fetch
     |-- cm tools        MCP client: list_projects, create_card, create_playbook,
     |                   add_playbook_entry, get_playbook, get_card, run_playbook
     |-- control tools   ask_human (blocking), submit_artifact (terminal)
     |
store (internal/store, SQLite)      blueprints, stage runs, artifacts, questions, gates,
                                    transcripts, events, sources, ledger, emissions,
                                    outcomes, lessons
```

- **The controller has one interface.** Commands in, events out, behind one
  adapter: `internal/webhook`, CM's chat-backend protocol. CM starts a session
  with a payload, the user's messages arrive on `/message`, events stream back
  as log entries over SSE and the panel renders them as messages and tool
  calls. Questions, gates, the gate plan and proposed lessons are emitted as
  typed entries with a structured payload; the panel renders them as plain
  messages until it has buttons, and the reply is a message either way, so
  buttons later change nothing on the architect's side.
- **Every blueprint is a CM session, started by CM.** The start payload is the
  `ChatStartPayload` with its `architect` block: the architect takes its LLM endpoint
  (fail-closed, no local fallback), MCP key, model and git-credentials token,
  and ignores `resume` and the worker image. It reopens the blueprint by id and
  runs no container. An end-session call detaches the client; the blueprint
  persists. A message that arrives while no question is pending is queued as a
  note for the next stage and logged, never dropped.
- **A question blocks the run.** `ask_human` is an ordinary tool: its
  execution waits until the attached client answers or, under `auto`, until a
  responder run returns. The model's run continues with the answer as the tool
  result. No model is running while a human thinks; the run is parked on a
  channel.
- **A run ends on `submit_artifact`**, a terminal tool carrying the markdown
  body and the stage's structured yaml block. The controller stores the
  artifact and the full transcript.
- **Critique loop inside a stage.** On submit, the mechanical checks run. On
  failure the controller resumes the same conversation in process (the run's
  messages plus the failures as a user message) for another round. When they
  pass and the pack declares a reviewer for the stage, a reviewer run (next
  stage's persona, no access to the builder's transcript) scores the artifact
  1-5. Below the pack's threshold its feedback resumes the builder the same
  way. Three rounds per attempt, then the stage stops and waits for a human
  regardless of policy. Each round is a harness run with its own cap; the
  ledger sums them.
- **A gate waits between runs.** The controller holds the stage's artifact and
  asks the client. Nothing runs meanwhile.
- **A human rejection starts a new attempt**: a fresh run with the previous
  artifact and the feedback as input.
- **Across stages only artifacts carry forward**, never transcripts. Each stage
  consumes the artifacts the pack names.
- **Crash recovery.** Artifacts are stored per stage, so after a dead process
  reopening the blueprint's session re-runs the open stage from its inputs. No
  transcript is ever resumed from the store; transcripts exist for audit.

## Data model

SQLite (`modernc.org/sqlite`), one file. Append-only where marked.

| Table         | Holds                                                                                                                        |
| ------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| blueprints    | id, title, target CM projects, pack name and version, policy overrides, state, cost cap, model price as CM sent it, CM playbook id after emission         |
| stage_runs    | blueprint, stage, attempt, round, status, model, started, ended, cost, token buckets                                                          |
| artifacts     | append-only: blueprint, stage, version, markdown body, structured yaml block, check results, reviewer score                   |
| questions     | append-only: run, number, text, options, answer, responder (`human`, `auto`), assumption flag, verified flag                   |
| gates         | append-only: blueprint, stage, attempt, decision, feedback, responder, decided at                                             |
| transcripts   | append-only: run, full message history as JSON, redacted                                                                     |
| events        | append-only: time, blueprint, run, kind, payload                                                                              |
| sources       | id (`S<n>` per blueprint), kind (`repo`, `url`, `file`; later `jira`), identity, version (sha, content hash), fetched at, first used by stage |
| ledger        | per-run cost rows summed per blueprint                                                                                        |
| emissions     | draft card id to CM project and card id; playbook id; created at                                                              |
| outcomes      | per emitted card: final state, review attempts, parked reason, cost, finding text, proposed lesson                            |
| lessons       | text, rationale, evidence card ids, scope (pack or project), state (`proposed`, `active`, `rejected`), decided by             |

Schema bumps are one-way: an older binary refuses a newer file.

**Audit.** Every action produces an event: blueprint created, run started
(model, pack version, persona, config snapshot), tool call made, question
asked, answer given, assumption recorded, check passed or failed, reviewer
score, gate decided, artifact version stored, emission call and CM's response,
export written, playbook run started, outcome recorded, lesson proposed or
decided. Tables marked append-only have no update or delete path in code, and
tests prove it. Secrets are redacted before anything is stored.

**Source tags.** Every tool result that reads something external is prefixed
with its source id (`S7`). Claims in artifact sections the pack marks as
grounded carry a tag: `[S7]` for a source, `[Q3]` for an answered question,
`[assumption]` for a decision nobody grounded. An untagged claim in a grounded
section fails the mechanical check. Sign-off re-reads `sources` and flags every
repo whose default branch moved since grounding.

## Stages

The default pack. Personas: product owner (PO), system designer (SD), senior
developer (DEV), reviewer (REV).

| #  | Stage           | Persona | Consumes          | Produces                                                                                              | Gate      |
| -- | --------------- | ------- | ----------------- | ----------------------------------------------------------------------------------------------------- | --------- |
| 1  | Intake          | PO      | the need          | need: problem, value, scope in and out, measurable outcomes; the expected shape and the gate plan     | questions |
| 2  | Slicing         | PO      | need              | slices, each delivering standalone value; one slice is fine                                           | yes; folds into 3 when there is one slice |
| 3  | Solution design | SD      | need, slices      | per slice: projects involved, decisions with rejected options, cross-project contracts               | yes       |
| 4  | Grounding       | DEV     | design            | per project: files, symbols and patterns verified at a sha, verify command, instruction file presence | no        |
| 5  | Acceptance      | DEV     | design, grounding | per slice: criteria checkable inside one repo; human and live checks split into manual steps         | `auto` by default |
| 6  | Decomposition   | DEV     | all above         | draft cards, `depends_on` per project, playbook order                                                 | no        |
| 7  | Readiness       | REV     | cards             | contract check per card; failures loop back to 6, three rounds max                                    | no        |
| 8  | Sign-off        | human   | everything        | approval; then emission                                                                               | yes       |
| -  | Outcomes        | REV     | emitted cards     | one finding per struggled card and a proposed lesson                                                  | lessons   |

The embedded pack ships the stages as they are built; at present it holds intake.

- Intake ends by stating the shape it expects (slices, projects involved) and
  the gate plan in effect, so the operator can change the overrides before
  grounding spends anything.
- A gate folds when the pack's condition on its artifact holds: the slicing
  gate folds into the solution design gate when there is exactly one slice.
  Folding is decided by the artifact, never by an estimate of size.
- Reviewer runs are declared per stage in the pack. The default pack runs them
  for solution design, acceptance and readiness only, so a small need costs
  about eight runs.
- A small need therefore touches the human twice, at solution design and
  sign-off, plus at most a question or two. A need that slices into three gets
  every gate without anyone classifying it.
- A cross-project contract is copied into the cards on both sides of the
  boundary.
- Human-only and live-environment checks become manual playbook entries, never
  card criteria.
- Mechanical checks per stage come from the pack: required sections, source
  tags in grounded sections, the `depends_on` graph acyclic, every acceptance
  criterion of a slice mapped to exactly one card.

### Packs

A pack is a directory:

```
pack.yaml          stage order; per stage: persona, consumes, produces, tools,
                   default gate policy, gate fold condition, checks,
                   reviewer on or off and its threshold
personas/*.md      role text, one file per persona
stages/*.md        instructions, one file per stage
card.md            the card body template
lists.yaml         data for mechanical checks: required sections, stripped headings,
                   defect phrases, maintenance labels and verbs, forbidden content
```

- The default pack is embedded in the binary. The `pack` config key
  (`CONTEXTMATRIX_ARCHITECT_PACK`) names a pack directory that overrides it.
  The pack name and version are logged with every run.
- No include mechanism. Each file stands alone; shared text is duplicated. If
  that hurts, build-time expansion comes later.
- Gate and question policies come from the pack, overridable per blueprint.
- A lesson that extends a list lands as a pack overlay stored in SQLite and
  applied at load time, so a lesson never needs a code change.
- Pack prose is never tested. Tests check only that a pack parses and that
  every stage, persona and list reference resolves.

## Card contract

Evidence: 341 agent-run cards on the operator's main boards repo plus 161 on a
second instance, read against the `contextmatrix-agent` prompts. Struggle tracks
size and scope, not wording. Under the current harness, struggle starts once the
planner splits a card into two or more subtasks (26% against 6% below that).
Cards with both an acceptance-criteria section and a project verify command
struggled 8% of the time, against 25% when either was missing. The data is thin
(18 struggled cards on the main repo); Outcomes re-measures it.

### What the agent does with a card

Verified against `contextmatrix-agent` at harness v0.25.0.

- The body goes verbatim into every prompt, with these sections stripped from
  all of them: Diagnosis, Design, Plan, Discussion, Verify Command, Review
  Approval, PR Gates, Execute Discussions, Best-of-N Report, and any heading
  starting with Review Findings or Copilot Review. `## Split` and
  `## Unreachable Criteria` are not stripped.
- A human-written `## Diagnosis` is discarded; the planner sees only a freshly
  generated one.
- `## Design` reaches only the planner. The coder sees it only through
  planner-written subtask bodies. The reviewer never sees it.
- `## Plan` is appended to the planner input as background. The planner always
  re-plans.
- Routing is substring-based. A card is routed to a root-cause pass when its
  type is `bug`, a label is `bug` or `bugfix`, the first title word is one of
  fix, bugfix, repair, resolve, investigate, debug, or the body contains any
  of: "doesn't work", "does not work", "is broken", "throws", "crashes",
  "fails when", "unexpected behavior", "regression", "stack trace", "panic",
  "error:". Maintenance routing needs both a label in {simple, chore,
  dependencies, infra} and a first title word in {bump, update, rename, move,
  pin, upgrade, downgrade}, and outranks bug.
- Priority and `depends_on` are not read during a run.
- Review blocks only on an unmet criterion, a defect, a vacuous test, a
  vulnerability or out-of-scope work.
- The task-skill menu reaches the coder, fix, document and specialist prompts,
  not the planner.
- The root `AGENTS.md`, else `CLAUDE.md`, is injected verbatim into every phase.

These facts are data in `lists.yaml` and must be re-verified on every agent
release.

### Rules

**Shape**

- One deliverable, sized for one pull request and at most two planner subtasks.
- No bundled fixes, no optional or "phase 2" scope.
- Every decision is made. Never "A or B".

**Fields**

- Title starts with an imperative verb. Not a maintenance verb unless the card
  is mechanical work with a maintenance label.
- Type `bug` only for a real defect.
- `skills` names the task skills the work needs.
- `depends_on` within the project only.

**Body** (plain `##` sections)

| Section             | Content                                                                                                    |
| ------------------- | ---------------------------------------------------------------------------------------------------------- |
| Context             | Why, written into the card. For a bug, the root-cause hypothesis and its evidence.                          |
| Where               | Files, functions and types in this repo, verified at a sha. For wiring changes, every call site.            |
| Change              | The decided approach. Binding constraints are restated in the criteria, since nothing else reaches review. |
| Acceptance criteria | Numbered. Each is a test or command checkable in the working tree.                                         |
| Tests expected      | The test cases to write.                                                                                   |
| Out of scope        | Explicit non-goals.                                                                                        |

**Never**

- A `## Diagnosis` or `## Design` section: the first is discarded, the second
  is invisible to the coder and reviewer.
- Criteria under any heading the agent strips.
- A defect phrase anywhere in a feature card, including negated ("does not
  panic").
- A link as the only source of a fact.
- Other repos, clusters, deploys, live performance or pull-request comments in
  scope or criteria.
- Destructive self-checks, such as "revert the change and confirm the tests
  fail".
- Polish or hardening requests as criteria: review does not block on them.

**Target project prerequisites**

- A `verify.command` in the project's `.board.yaml`. The architect refuses to
  emit into a project without one.
- A root `AGENTS.md` or `CLAUDE.md`.
- A repository URL on the project.

### Readiness checks

| Check                                                              | How                        |
| ------------------------------------------------------------------ | -------------------------- |
| Required sections present, criteria numbered                       | mechanical                 |
| No local paths, other repos, cluster or deploy references          | mechanical                 |
| No defect phrase in a feature card; no criteria under a stripped heading | mechanical (lists.yaml) |
| Every path and symbol in Where exists at the recorded sha          | mechanical (code tool)     |
| `depends_on` graph acyclic within each project                     | mechanical                 |
| Every acceptance criterion of the slice maps to exactly one card   | mechanical                 |
| Target project declares a verify command and a repo                | mechanical (`list_projects`) |
| One deliverable, no open decisions, no optional scope              | model-judged               |
| Estimated planner split of two subtasks or fewer                   | model-judged               |
| "Could an agent with only this repo finish this card?"             | model-judged, adversarial  |

The contract lives here: the default pack's `card.md` and `lists.yaml` plus
`internal/contract`. The agent is the consumer and its prompts are the ground
truth the lists are copied from.

## Interaction

Every question and gate has a responder policy from the pack, overridable per
blueprint.

| Policy  | Questions                                                                                   | Gates                                                                 |
| ------- | ------------------------------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `human` | Shown in the CM session with options; the run waits for the reply.                           | Artifact shown; a reply beginning with approve or reject, feedback after it, until CM grows gate buttons. |
| `auto`  | A responder run answers. The answer is recorded as an `[assumption]` with responder `auto`. | A reviewer run scores; approved above the threshold, else rejected with feedback, three rounds, then waits for a human. |

**The responder run** is a separate harness run with the persona that would
have answered (the product owner for intake questions, the system designer for
design questions). It gets the need, the artifacts so far and the read tools,
never the asker's transcript, so the asker cannot answer itself conveniently.
It returns the answer and its reasoning. When it cannot ground the answer, it
still decides and marks the assumption `unverified`. Sign-off lists every
unverified assumption. The export and the playbook description list every
assumption, so whoever reviews the result sees which decisions the AI made.

Later policy: `jira` (question posted as a comment; after two reminders
without a reply it falls through to `auto`, logged as such).

### Modes

The mode is chosen in the panel's new-session dialog and recorded on the
blueprint.

- **Default**: policies as the pack sets them: `human` at solution design and
  sign-off, `auto` at acceptance. A reply to the gate-plan message at the end
  of intake changes a gate's policy for this blueprint.
- **`auto`**: every policy `auto`. Stops before emission. Cards auto-vet in
  CM on the non-Jira path, so this stop is the only human backstop.
- **`auto+emit`**: as above, then emits the playbook and stops.
- **`yolo`**: as `auto+emit`, then starts the run. A single card is
  started with `run_card` and ends as one pull request to the project's default
  branch under the card's own settings. Several cards are started with
  `run_playbook`, which makes the playbook runnable and plays it; every card
  then runs with `autonomous`, `create_pr`, `await_ci` and `merge_pr` on, each
  pull request merges into `playbook/<id>` on CI pass, and a compare link
  offers one final pull request to the default branch. That final merge stays
  human. The session reports the run's progress and end.

The architect's cost cap bounds the architect. The CM runs a yolo blueprint
starts are bounded by CM's per-card guardrails, not by the architect.

## Tools

| Group    | Tools                                                                                                   | Notes                                                                                                                                             |
| -------- | ------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| code     | `repo_info`; harness `read`, `grep`, `glob` jailed to the blueprint's checkout                          | The server keeps one bare mirror per repo (URL from `list_projects`), fetched with go-git under a per-repo lock at the start of grounding, and one checkout per blueprint from that mirror at the default-branch sha it records as a source. Blueprints on the same project never share a tree, so parallel runs never see each other's fetches, and the recorded sha is the one readiness verifies and sign-off compares against. It never reads a developer's own checkout. Credentials come per repo from CM's `GET /api/worker/git-credentials` with the session's token, so project credential bindings apply. `repo_info` returns default branch, sha, instruction file presence and the project's verify command. |
| research | `web_fetch`                                                                                             | Readable text, recorded as a source. A search provider is deferred.                                                                               |
| cm       | `list_projects`, `create_card`, `create_playbook`, `add_playbook_entry`, `get_playbook`, `get_card`, `run_card`, `run_playbook` | MCP client over Streamable HTTP with the MCP key from the session's start payload and a configured caller identity (default `human:architect`). The chat backend's bridge, copied and generalised with per-server tool prefixes and an allowlist, so a second MCP server plugs in the same way. |
| control  | `ask_human`, `submit_artifact`                                                                          | `ask_human` blocks: question, options and context in; the answer out as the tool result. `submit_artifact` is terminal: the markdown body and the structured yaml block the stage's schema requires. |

Every tool result passes the harness redactor for every secret the process
holds.

## Emission

At sign-off, in order:

1. **Preflight.** Every target project exists in CM, declares a verify command
   and a repo URL; grounding found an instruction file; no source repo's
   default branch has moved since grounding (a moved branch needs a re-ground
   or an explicit, logged override); no draft card's title matches a card in
   another runnable playbook.
2. **Cards**, per project in dependency order so `depends_on` carries real ids.
   Each created id is stored in `emissions` against its draft, which makes a
   re-run after a partial failure skip what exists. A warning from
   `create_card` is a readiness miss: stop and log, never continue blind.
3. **Playbook**, when there are two or more cards, in blueprint order, with
   manual entries for human checks, live checks and cross-project gates. Its
   description carries the blueprint summary and the assumption list. A
   single card carries the summary and assumptions in its own Context.
4. Ids stored in `emissions`, state `emitted`. Running is a human action in
   CM, or `run_card` and `run_playbook` in yolo mode.

The architect does not use CM's Jira import for cards. Its traps hit exactly
what the contract tunes: imported titles start with the issue key, no labels
are set, and an appended epic-context section is not stripped.

## Learning

The Outcomes stage closes the loop from emitted cards back to the contract.

- **Trigger**: an `outcomes` reply in the session, or the server noticing the
  run has ended.
- **Input** per card: the card as emitted, the card as it ended (`get_card`
  returns the body with the agent's appended review findings, plan and log),
  and the run signals: final state, review attempts, parked or stalled reason,
  cost.
- **Output**: one finding per struggled card naming which contract rule or
  readiness check should have caught it, with evidence, and a proposed lesson.
- **Lessons** are proposed by Outcomes as a message in the session and decided
  by a human replying approve or reject to it. Active lessons are injected into the
  decomposition and readiness prompts. A lesson that extends a check list
  becomes a pack overlay. Lessons are scoped to the pack or to one project.
- Reading outcomes needs no CM change: the architect knows its card ids. A CM
  card-creator field comes later, to compare architect cards with human-written
  ones across the whole board.

## Sessions

The only surface is a blueprint session in CM's chat panel. The new-session
dialog picks the target projects, the mode and a cost cap. The need is stated
there, questions and gates are answered there, and the stage's progress, tool
calls and cost stream into it as log entries. The binary has one command,
`architect serve`, plus the `config defaults|validate` helpers.

Replies the server understands, besides answers to questions:

| Reply                         | Effect                                                                 |
| ----------------------------- | ---------------------------------------------------------------------- |
| `approve`, `reject <feedback>` | Decides the open gate, or the proposed lesson the reply answers.        |
| `gate <stage> human\|auto`    | Changes a gate's policy for this blueprint; offered at the gate plan.   |
| `export`                      | Writes the markdown bundle (artifacts, decisions, questions, events, transcripts) under the server's state directory, and commits it to the repo when the project opted in. |
| `outcomes`                    | Runs the Outcomes stage on the emitted cards.                           |
| `lessons`                     | Lists lessons and their state.                                          |
| `abort`                       | Ends the open run; the blueprint stays and can be reopened.             |

- A run does not start if it would breach the blueprint's cost cap.
- Reopening a session makes CM send a start for it; a stage interrupted by a
  server restart is re-run from its inputs. Completed artifacts are never
  lost.
- A project may opt into automatic export in config: a target repo, branch and
  directory; the bundle is committed there at each gate or at sign-off. Off by
  default.
- A message that arrives while no question or gate is open is queued as a
  note for the next stage and logged.

## Deployment

- **One server.** `architect serve` runs in a container next to the agent and
  chat backends. It is configured in CM as a backend of kind `architect` with
  its URL and an HMAC key, exactly like the other two.
- **Provisioned per session by CM.** The start payload carries the LLM
  endpoint, the MCP key, the model and the git-credentials token. The
  architect holds none of these in config or env, and refuses a start without
  the endpoint, as the chat backend does.
- **Code.** The server checks out each target project's repo from the URL CM
  reports, into its own workspace cache, with credentials fetched per repo from
  CM. Nothing is checked out on a client.
- **Container needs.** A Go binary image with no Docker socket (it spawns
  nothing), a volume for the state directory, and outbound access to CM, the
  LLM endpoint and the git host. Env: the CM base URL and the backend HMAC key.
- **Client.** The panel, through CM. Local use is `architect serve` on the
  laptop pointed at a CM instance. There is no CM-less mode.

## Runtime

- Go 1.27, `contextmatrix-harness` v0.25.0 (loop, llm client, jailed read
  tools, Skill tool, redact, events), `contextmatrix-backendkit` v0.7.0
  (`webhookcore`, `logbridge`, `metrics`) and `contextmatrix-protocol` v0.22.0
  for the CM-facing side, go-git for checkouts, `modernc.org/sqlite`, the Go
  MCP SDK as a client, cobra and koanf as in the agent.
- Server config file plus `CONTEXTMATRIX_ARCHITECT_*` env overrides: CM base
  URL, backend HMAC key, listen address, state directory, pack, per-project
  export opt-ins, and optional per-persona model overrides (slugs only).
  Secrets reach only their client and never a log, error, artifact or store
  row.
- Models: the start payload's model is the default for every persona; config
  may override per persona. Priors-based selection through
  `contextmatrix-protocol`'s `selection` package comes later.
- Cost: the harness per-run cap plus the blueprint ledger across runs. A
  critique round re-bills the conversation so far; the ledger counts it.
- Long intake dialogs rely on the harness's in-run compaction. Whether that is
  enough is checked in slice 3.
- State dir: `architect.db` and `workspaces/<repo>/mirror/` plus
  `workspaces/<repo>/<blueprint>/`. A blueprint's checkout is removed at
  emission and by a sweep for blueprints untouched for a configured number
  of days; mirrors stay. All of it is a disposable cache. The mirrors and checkouts arrive with grounding.
- LLM endpoint: whatever CM provisions, OpenRouter or any OpenAI-compatible
  endpoint. Never name a gateway operator in repo artifacts.
- Builds on macOS too: no filenames differing only by case.

## Package layout

```
cmd/architect
internal/blueprint   state machine, gates, responder policy, cost cap; the command and event interface
internal/pack        pack loading, overlays, lists
internal/stage       stage runner: prompt assembly, tool sets, output schema, critique loop
internal/contract    card contract checks, mechanical and model-judged
internal/store       SQLite: schema, append-only tables, queries
internal/code        workspace checkout and repo_info
internal/research    web_fetch
internal/cm          MCP client and emission
internal/outcomes    outcomes stage and lessons
internal/export      markdown rendering into the state directory and the opted-in repo
internal/config
internal/webhook     CM-facing chat-backend protocol on backendkit webhookcore and logbridge; reply parsing
```

## Changes needed in CM

| Change                                                                                             | Needed for          |
| -------------------------------------------------------------------------------------------------- | ------------------- |
| `run_playbook` MCP tool: makes the playbook runnable if it is not, then plays it; `human:` caller gate like `run_card` | yolo with several cards (slice 2); a single card uses the existing `run_card` |
| An `architect` backend kind: config entry with URL and key, its own callback prefix, session start dispatched by kind in the chat manager with the existing `ChatStartPayload` | slice 0 |
| Chat sessions carry a kind and a blueprint binding instead of only a project; a one-way ops.db schema bump | slice 0 |
| Panel: a new-session dialog for blueprints with a multi-project picker, mode and cost cap | slice 0 |
| The git-credentials endpoint treats an attached architect session as live | slice 1 |
| Panel: typed question, gate, gate-plan and lesson entries rendered with buttons instead of as plain messages | slice 3 |
| Panel: a gate inbox and a blueprint view (stages, assumptions, cards with readiness, cost), read from the architect through a signed endpoint | later |
| A card-creator field or creation log entry, so architect cards can be compared with human-written ones and flagged for vetting | later |
| `source` input on `create_card` and `create_playbook`, validated against `project_map`              | Jira mirror (later) |
| Endpoint returning a selection context for non-backend consumers                                     | later               |
| Richer writeback: pull-request link, review state, merge, epic transition                           | Jira mirror (later) |

## Delivery slices

Ordered so a real card comes out as early as possible.

0. **Scaffold.** In CM: the `architect` backend kind, session binding and the
   panel's new-session dialog for blueprints. In the architect: git repo, Go
   module, Makefile and `AGENTS.md` mirroring CM's conventions, config,
   SQLite store with `events` and `transcripts`, `serve` receiving a session
   start from CM, the controller interface, a stage runner that runs one
   harness step on the stated need with the payload's endpoint, and its
   events streaming into the panel.
1. **Cards from a pasted need and design.** Grounding, decomposition and
   readiness with the data-driven contract checks, code tools, the `export`
   reply. Output is a report in the session. This tests the core of the
   design first.
2. **Emission.** Cards and playbook over CM MCP, their status reported back
   into the session, yolo with `run_card` for one card and, after the CM
   change, `run_playbook` for several. First real blueprints: a one-card
   feature, then the Kubernetes example, on the operator's own projects.
3. **Dialog stages.** Intake, slicing, solution design, acceptance; `human`
   and `auto` responders; gates. In CM: typed question and gate entries
   rendered with buttons.
4. **Outcomes and lessons**, with pack overlays.
5. **Later**: a gate inbox and a blueprint view in CM's web UI;
   seeding a blueprint from an existing CM card, in the same adapter shape as
   the Jira seed; Jira seed and mirror (decide REST against Atlassian MCP then,
   see open questions); search provider; composer; model selection via
   `selection`.

## Testing

- **LLM**: a scripted OpenAI-compatible `httptest` server behind the harness
  client, as the agent does. No live model in `make test`.
- **Contract checks**: pure functions, table-driven over fixture cards, good
  and bad, including the routing lists.
- **Store**: temp SQLite. Tests prove no update or delete path exists for
  append-only tables.
- **Controller**: driven through its command and event interface, table-driven
  state transitions. A question test blocks a run on `ask_human`, answers
  through the interface and asserts the run continues with the answer.
- **Critique loop**: a submit that fails a mechanical check resumes the same
  conversation with the failures, and the cap stops it after three rounds.
- **Emission**: an in-test MCP server asserting call order, and that a re-run
  after a simulated partial failure creates nothing twice.
- **Webhook**: an in-test CM that signs requests starts a session with a
  payload, sends a message as the answer to a pending question, sends each
  reply form and asserts its effect, and reads the SSE log entries; an
  unsigned request and a start without an LLM endpoint are refused.
- **End to end**: one run driven through the webhook protocol against a
  fixture git repo in `t.TempDir()`, producing a report.
- **Pack**: parses, references resolve. Never its prose.

## Open questions

- Jira mirror transport: native REST (small, CM has half the client, markdown
  to ADF in Go) against Atlassian's MCP server through the bridge (markdown
  in, no client to own, but auth may be browser OAuth only). Either way the
  credential should be provisioned by CM, which already holds the Jira site and
  token, rather than configured on the architect. Auth for a headless service
  decides it; verify at the Jira slice.
- Web search provider and its data-handling terms.
- Data handling: code and ticket content go through the LLM endpoint. Personal
  data in tickets needs a policy before the Jira slice.
