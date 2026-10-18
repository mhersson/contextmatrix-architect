# contextmatrix-architect - design

Status: agreed design, 2026-10-15. Slices 0 to 3 are implemented: slice 2 signs off ready cards, emits them and a playbook into CM, starts them in yolo mode and reports their run, and slice 3 adds the dialog stages (intake's questions, slicing, solution design, acceptance) with a responder policy per stage, the human and auto responders, reviewer runs with the critique loop, gates with slicing's folding into solution design, CM prompts, notes that reach later stages and assumptions with grounding's verdicts. Outcomes, lessons and the export's commit to a repository are slice 4; the export writes the bundle under the state directory.

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
it, unless a card's repository has no commits: the run then waits for a first
commit and an `approve`.

## Vocabulary

| Term          | Meaning                                                                                             |
| ------------- | --------------------------------------------------------------------------------------------------- |
| architect     | This tool.                                                                                          |
| blueprint     | The product for one need: artifacts, decisions, cards and playbook order. Lands in CM as a playbook, or one card. |
| stage         | One unit of AI work producing one artifact. Runs as one or more short harness runs.                 |
| gate          | A decision point after a stage: approve, or reject with feedback.                                   |
| question      | Something a stage cannot decide alone, asked in a round.                                            |
| round         | The questions a stage asks together, and its assumptions as the last tab: one tab each in the session, answered in one sitting or in batches, stored until every question has an answer and every assumption a decision. |
| responder     | Who answers a question or decides a gate: a human in the CM session, or the AI (`auto`).            |
| persona       | The role a run plays: product owner, system designer, senior developer, reviewer.                   |
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
| CM's chat panel is the only client                        | The architect implements CM's chat-backend protocol as a third backend kind, so CM's existing panel is where the need is stated, questions and gates are answered, and side operations (export, outcomes, lessons) are requested by reply. Same look and feel, session list, auth and browser for free. No UI, no CLI beyond `contextmatrix-architect serve` and the `config defaults` and `config validate` helpers contextmatrix-setup drives, no client API. |
| One controller process                                    | `contextmatrix-architect serve` alone owns the store, the workspaces and the harness runs, and every session originates in CM. One record, one place code is checked out, no laptop copy of anything. |
| Provisioned by CM, like the agent and chat backends      | Every blueprint is a CM session. The start payload delivers the LLM endpoint, the MCP key, the model, a git-credentials token and, when research is on, the search provider and its key. The architect's own config holds no secret but the backend HMAC key, and there is no CM-less mode. |
| Questions in stored rounds; a stage resumes its stored conversation | A big need takes a long interview, answered in one sitting or in batches over days. Posting a round ends the run, so nothing waits in memory and End, a reopen or a restart loses nothing; once every question has an answer and every assumption a decision the stage resumes the conversation that asked, with the answers as its next message, so the model keeps its reasoning, and the prompt cache is warm when the answer comes quickly. |
| SQLite is the record; markdown is a rendering             | One store for artifacts, gates, transcripts, audit and cost. Committing per-stage markdown into code repos bloats them fast; an opt-in automatic export serves teams that review in pull requests. |
| Everything is logged                                      | Every run's full transcript (less web search result lists) and an append-only event log. The append-only tables are never updated or deleted. The export is a complete audit record, less the web search result lists the next row keeps out. |
| Search result lists are never stored                      | The search provider's terms forbid storing or caching results beyond transient use, and no storage rights are bought. The model sees a list in the run that searched; transcripts, events and exports keep the query and the result count. The pages `web_fetch` reads are the recorded sources, so a claim cites a page the architect read itself. A deliberate, permanent exception, with no switch to record lists. |
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
     |  session start, /message, /logs SSE, end-session, the blueprint view, artifacts and live SSE)
     |  commands in: answer, approve, reject, abort, note, export, gate <stage> human|auto, outcomes, lesson decision
     |  events out: stage progress, tool calls, questions, gates, cost
controller (internal/blueprint)     blueprint state machine, gates, responder policy, cost cap
     |
stage runner (internal/stage)       one harness.Run per round: persona + stage prompt +
     |                              pack rules + lessons + tool set + output schema
     |-- code tools      read-only tree per project from a bare mirror, at a recorded sha;
     |                   harness read/grep/glob jailed to it; repo_info
     |-- research tools  web_search, web_fetch behind the leak guard;
     |                   public addresses only
     |-- cm tools        read-only MCP reads for grounding and readiness:
     |                   cm_list_projects, cm_list_cards, cm_get_card, cm_get_playbook
     |-- control tools   ask_questions (posts a round, ends the run),
     |                   answer_chat (replies in a round's chat, ends the run),
     |                   submit_artifact (terminal)
     |
store (internal/store, SQLite)      blueprints, stage runs, artifacts, questions, gates,
                                    transcripts, events, sources, ledger, emissions,
                                    outcomes, lessons
```

- **The controller has one interface.** Commands in, events out, behind one
  adapter: `internal/webhook`, CM's chat-backend protocol. CM starts a session
  with a payload, the user's messages arrive on `/message`, events stream back
  as log entries over SSE and the panel renders them as messages and tool
  calls. Text a person reads names a stage by its display name, its id with
  underscores as spaces and the first letter upper case (`Solution design`);
  prompt ids, event payloads, markers and the `gate <stage> human` command
  keep the raw id. A gate a human answers is a text entry carrying a prompt
  (`LogEntry.Prompt`): actions, each a label and the exact reply text, which
  the panel shows as buttons, the markdown staying the readable fallback. A
  gate's, the sign-off's, a stopped emission's or a run start's prompt
  carries the entry without its reply instruction as its text, and the kind
  `gate`. A click sends the reply as an ordinary message, so the
  architect's reply parsing is the same for a click and a typed answer.
  A stage asks in rounds: a round is a text
  entry whose prompt has the kind `round`, the id `round:<n>` and the round
  itself (`LogPrompt.Round`: each question's number, title, text, why it
  matters, options with summary, pros, cons and sources, the recommended
  one, whether its stage researched (`research_off` set when it did not),
  and the answers stored so far). It is published again with the same
  id whenever it changes, closed once its stage resumes, and the panel
  shows the newest frame. Its reply carries the answers
  (`MessagePayload.Round`); the reply's content is only a readable summary,
  never parsed as a command. A reply carries every answer the panel holds
  and replaces per question, and a reply for a round that takes no answers
  stores nothing and says so.
  Round 1 of each attempt of a stage run is a stage divider
  (`LogEntry.Marker` of kind `stage`, a system entry), and a stored
  artifact is a sheet reference (kind `artifact`, a text entry) to that
  stage and version in place of its body. Its one-line note says what is
  known when it is stored: the persona that drew it, the findings a
  revision was given, a loop stage's round, or that its checks refused it
  until round 3. Reviewer verdicts and gate decisions stay prose entries.
  Each marker keeps a plain fallback line in `Content`, which a CM without
  markers shows instead.
  Rounds are `round:<n>` and their questions `Q<n>`, gates
  `gate:<stage>:v<n>`, the sign-off `gate:signoff:v<n>`, and a stopped
  emission or run start `emission:v<n>`.
  A gate the auto policy approves never leaves a prompt open, and the gate
  plan is plain text. A gate or the sign-off stays open across a reply that
  matches none of its actions, so it is never published again.
- **Every blueprint is a CM session, started by CM.** The start payload is the
  `ChatStartPayload` with its `architect` block: the architect takes its LLM
  endpoint (fail-closed, no local fallback), MCP key, model and
  git-credentials token, and ignores `resume` and the worker image. It reopens
  the blueprint by id and runs no container. An end-session call detaches the
  client; the blueprint persists. A message that arrives after the need,
  while no human's round waits (then it is chat about the round), and that
  decides no open gate is logged as a note, never dropped. Every model run
  that starts later, reviewer runs included, gets every note so far in its
  task, with the stage that was open when it came, except a bare `approve`
  or `reject`; a builder round that resumes a
  conversation gets the notes that conversation has not seen. Entries
  published before CM's consumer connects, or while it is reconnecting, are
  retained on the server and replayed to it once, ahead of live entries.
- **A round ends the run.** `ask_questions` stores the round, numbers it
  `round:<n>` and its questions `Q<n>` after every question so far, shows it,
  and ends the harness run with the status `asked`. No model runs and nothing waits in memory while a human thinks.
  Answers sent in batches are stored as they come. Once every question has
  an answer and every assumption on its tab a decision the round closes:
  each answer is recorded in `questions`, each accepted assumption on the
  round itself (see Interaction), and a
  continuation runs: a stage run of the same attempt and builder round that
  resumes the stored conversation of the run that asked, or of the chat run
  that replied last, with the answers, the chat it has not answered and the
  notes that conversation has not seen as its next message. It may ask a
  follow-up round. Without that conversation a first build starts over in
  its attempt, given every answered question, and a builder round is run
  again by its critique. A message while a human's round waits is chat
  about it: it is stored on the round, and a chat run, a stage run of the
  same attempt and builder round, resumes the stored conversation with the
  messages it has not seen and replies with `answer_chat`. It may revise a
  question or add one; a question whose options change loses its answer,
  and the others keep theirs. It may also replace the assumptions tab with
  a non-empty list (an empty or non-list value is a tool error; leaving the key out keeps the tab); an
  assumption whose text is unchanged keeps its label and its decision, and
  a new one takes a label the round never issued. Messages
  written while a chat run runs are answered by the next, and a round whose
  last answer arrived meanwhile closes once it ends. The reply reaches the panel only in the round's
  chat; the chat run's text and its `answer_chat` call stay in the record.
  A chat run that fails or is aborted leaves its messages until the user
  writes or answers again: the chat runs again, or the continuation of the
  answered round gets them.
  Under `auto` a responder run answers each question inside the asking run
  (see Interaction), every assumption on the tab is accepted, the round is
  shown answered and read-only, and the stage continues at once.
- **A run ends on `submit_artifact`**, a terminal tool carrying the markdown
  body and the stage's structured yaml block. The controller stores the
  artifact and the full transcript (less web search result lists). A run that
  stops with text and no terminal call gets one follow-up run on its
  conversation. That run is offered only the run's terminal tools and made to
  call one (`tool_choice`, sent once more without it when the endpoint refuses
  that with a 400), with two turns, so a refused submit gets one retry. The
  ledger and the transcript cover both runs. If the follow-up also stops with
  text, the run failed. Reviewer and responder runs get the same follow-up.
- **Critique loop inside a stage.** On submit, the mechanical checks run
  inside the run: a failing submit is returned to the model as the tool
  result and it submits again. The third failing submit is accepted: cards
  with their findings, and the pipeline stops and reports; any other stage's
  artifact as it is, with the problems the checks still found kept in its
  data, shown at its gate and given to its reviewer. When the stage has a
  gate that does not fold, the reviewer the pack declares for it then judges
  the accepted artifact, given its own persona, the stage's inputs, the
  questions the stage got answered, the assumptions so far, the gate
  feedback its attempt addresses, the user's notes and the artifact, never
  the builder's transcript, and no tools. It scores the artifact 1-5, and
  the verdict is stored as an artifact of the pseudo-stage `<stage>.review`.
  Below the pack's threshold the findings, with the notes and answers the
  builder's conversation has not seen, resume that conversation from its
  stored transcript for another round of the same attempt. Three reviewer
  rounds per attempt, then the gate waits for a human regardless of policy.
  A passing verdict with findings resumes the builder once more the same
  way, a revision round of the same attempt, and the revised version goes to
  the gate without another review; the gate shows that it was revised and
  the version the reviewer scored. Each reviewer round is a harness run with
  its own cap, and so is a revision round; the ledger sums them.
  The round is given the findings numbered, and its submit may list the ones
  it addressed in its data as `addressed: [{finding, note}]`; a number the
  round was not given is refused like any check problem, and the third
  failing submit is accepted with its problems kept.
- **A gate waits between runs.** The controller holds the stage's artifact and
  asks the client. Nothing runs meanwhile.
- **A human rejection starts a new attempt**: a fresh run with the previous
  artifact and the feedback as input.
- **Across stages only artifacts carry forward**, never transcripts. Each stage
  consumes the artifacts the pack names.
- **Crash recovery.** Artifacts are stored per stage, so after a dead process
  reopening the blueprint's session re-runs the open stage from its inputs. A
  re-run is given the questions its stage already asked, their answers and who
  answered each (the user or the responder), so it does not ask them again,
  and the assumptions the user accepted on its tabs, each with its tag. A
  stage run is resumed from the store only as a continuation after a round;
  a builder round after a review, a low one's next round or a passing one's
  revision, resumes the transcript of the run that built the artifact.

## Data model

SQLite (`modernc.org/sqlite`), one file. Append-only where marked.

| Table         | Holds                                                                                                                        |
| ------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| blueprints    | id, title, target CM projects, pack name and version, state, cost cap, model price as CM sent it                                        |
| stage_runs    | blueprint, stage, kind (`stage`, `reviewer`, `responder`; only stage runs place the pipeline), attempt, round, status (running, done, asked, failed, interrupted, canceled), model, started, ended, cost, token buckets |
| artifacts     | append-only: blueprint, stage, version, markdown body, structured yaml block, check results; a reviewer's verdict (score, findings, the judged version, attempt and round) is an artifact of the pseudo-stage `<stage>.review` |
| questions     | append-only: blueprint, run, stage, number (`Q<n>` per blueprint), text, options, context, answer, responder (`human`, `auto`), assumption flag, grounded flag (the checked result: the responder's claim and a source tag its own tool calls returned), the answering message, answered at; written once: when its round closes, or at the responder's answer under auto, or at the answer for one asked alone before rounds; an assumption a human accepted on a tab is recorded on its round, not here |
| gates         | append-only, one row per decision at a stage's gate or the sign-off: blueprint, stage, the judged artifact's version, decision (approve, reject, fold, superseded), feedback, responder (human, auto), decided at |
| transcripts   | append-only: run, full message history as JSON, redacted, each web search result list replaced by its query and result count |
| events        | append-only: time, blueprint, run, kind, payload; a blueprint's policy overrides are its `policy` events, the latest per stage winning; every change of the blueprint's state is a `state` row, stored and never sent to the panel; every web search and page read is a `research` event (the query and result count, the URL and source tag, or the rule that refused it), and a session that runs a research stage without research says so once in a `research_off` event; a round is its `round` snapshots, the newest per round number winning |
| sources       | id (`S<n>` per blueprint), kind (`repo`, `url`, `file`, `cm`; later `jira`), identity, version (sha, content hash; none for `cm` or a repository with no commits), fetched at, first used by stage; a page `web_fetch` read is a `url` source versioned by the sha256 of its text, its title on its `research` event |
| ledger        | per-run cost rows summed per blueprint                                                                                        |
| emissions     | append-only: blueprint, kind (card, playbook, run), the draft card id, project, CM id, created at; one row per blueprint, kind and draft id |
| outcomes      | per emitted card: final state, review attempts, parked reason, cost, finding text, proposed lesson                            |
| lessons       | text, rationale, evidence card ids, scope (pack or project), state (`proposed`, `active`, `rejected`), decided by             |

Schema bumps are one-way: an older binary refuses a newer file.

**Audit.** Every action produces an event: blueprint created, state changed, run started
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
repo whose default branch moved since the checkout. A web search result is a
lead, not a source: it carries no tag, and only a page `web_fetch` read is
cited.

## Stages

The default pack. Personas: product owner (PO), system designer (SD), senior
developer (DEV), reviewer (REV).

| #  | Stage           | Persona | Consumes          | Produces                                                                                              | Gate      |
| -- | --------------- | ------- | ----------------- | ----------------------------------------------------------------------------------------------------- | --------- |
| 1  | Intake          | PO      | the need          | need: problem, value, scope in and out, measurable outcomes, the expected shape, reading the code      | questions |
| 2  | Slicing         | PO      | need              | slices, each delivering standalone value; one slice is fine; reads the code                             | yes; folds into 3 when there is one slice |
| 3  | Solution design | SD      | need, slices      | per slice: projects involved, decisions with rejected options and their basis, cross-project contracts; asks a question for each decision with real alternatives; reads the code | yes       |
| 4  | Grounding       | DEV     | design            | per project: files, symbols and patterns verified at a sha, verify command, instruction file presence | no        |
| 5  | Acceptance      | DEV     | design, grounding | per slice: criteria checkable inside one repo; human and live checks split into manual steps         | `auto` by default |
| 6  | Decomposition   | DEV     | all above         | draft cards with type and priority, `depends_on` per project, manual entries for human and live checks, a playbook title and order, and the setup for a project without a verify command or instruction file | no        |
| 7  | Readiness       | REV     | cards             | contract check per card; failures loop back to 6, three rounds max                                    | no        |
| 8  | Sign-off        | human   | everything        | approval; then emission                                                                               | yes       |
| -  | Outcomes        | REV     | emitted cards     | one finding per struggled card and a proposed lesson                                                  | lessons   |

The embedded pack holds every stage of the table but Sign-off and Outcomes. Sign-off is the controller's step after readiness (see Emission). Every stage is a model run; a design the user already has goes in the need or an answer, and solution design adopts it.

- Intake states the shape it expects (slices, projects involved). When its
  run completes, the controller posts the gate plan: who answers each
  stage's questions and decides its gate, for a gate that folds the gate
  it folds into and when, and the `gate <stage> human|auto` reply that
  changes it, so the operator can change a policy before solution design
  spends anything.
- Every stage that interviews (intake, slicing and solution design) reads the
  code, so it states what the code answers as a finding and asks only what
  neither the code nor the need settles.
- The first stage with the code tools, intake in the default
  pack, ends by telling the session what each target project lacks for an
  agent run: a verify command in CM (read again then), an instruction file
  at the root, or any commit. None of it blocks. Decomposition plans it as
  setup, and the user may set the verify command in CM meanwhile; the next
  decomposition submit reads it.
- Slicing's submit is checked: at least one slice, each with an id of its
  own, a title, and only the blueprint's target projects.
- Solution design's submit is checked: it has slices, every slice of slicing
  among them; every decision has a chosen option, a rejected option and a
  basis (`Q<n>`, `round<n>.A<k>`, `S<n>` or `assumption`); and every
  question it got answered and every assumption the user accepted on its tabs
  since the slicing it consumes was built is the basis of a decision or of a
  stated assumption; one from before a re-slice stays a basis it may cite.
- Acceptance's submit is checked: it has slices, every slice of slicing
  among them and each with at least one criterion; every criterion has an
  id of its own, a target project and its text, and every manual check an
  id of its own and its text. At decomposition, acceptance data that does
  not decode (`criteria_unreadable`) or defines no criterion
  (`criteria_empty`) is a contract finding, not a stalled run.
- A gate folds when the pack's condition on its artifact holds and the gate
  it folds into has the same policy: the slicing gate folds into the
  solution design gate when there is exactly one slice. Folding is decided
  by the artifact, never by an estimate of size, and is recorded as a `fold`
  decision when the pipeline passes slicing, so a later policy change cannot
  unfold it. A version its own reviewer judged never folds, and neither does
  the revision that verdict owed, even when the revision meets the
  condition: its gate was already put to its own reviewer. A folded stage's
  reviewer is skipped; the gate it folds into, and its reviewer, judge both;
  the reviewer gets the gate feedback each folded stage's attempt addresses
  too. Approve approves both, `reject <feedback>` runs solution design
  again, and `reject slicing <feedback>` runs slicing and everything after
  it again and records the design version it replaces as `superseded`, an
  audit row that decides nothing. The folded gate shows slicing's artifact
  and refers to the design on the sheet, as every gate does, and it
  waits for a human while either stage's policy is human; it approves both
  on its own only when both are auto.
- Reviewer runs are declared on every gated stage in the pack and skipped
  when its gate folds. The default pack declares them for slicing, solution
  design and acceptance; readiness is decomposition's reviewer and gets
  none. A one-slice need costs nine runs: intake, slicing, solution design
  and its review, grounding, acceptance and its review, decomposition and
  readiness; up to eleven when both reviews pass with findings, each adding
  a revision round, plus one continuation run per question round and one
  chat run per chat reply.
- In default mode a one-slice need touches the human at the rounds of
  intake, slicing and solution design (no limit when a human answers), the
  solution design gate with slicing folded into it, and the sign-off. A
  need that slices into several gets slicing's own gate as well, without
  anyone classifying it.
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
pack.yaml          stage order; per stage: persona, consumes,
                   produces, tools, checks, the responder policy for its questions and
                   gate (human, or auto by default), the gate, its fold condition, the
                   reviewer's persona and threshold, the auto responder's question cap, loop target
                   and rounds
personas/*.md      role text, one file per persona
stages/*.md        instructions, one file per stage, review.md for every reviewer run and responder.md for every responder run
card.md            the card body template
lists.yaml         data for mechanical checks: required sections, stripped headings,
                   defect phrases, maintenance labels and verbs, forbidden content,
                   dangling references
```

- The default pack is embedded in the binary. The `pack` config key
  (`CONTEXTMATRIX_ARCHITECT_PACK`) names a pack directory that overrides it.
  The pack name and version are logged with every run.
- A stage that checks cards must have the code tools, and its pack must
  carry `lists.yaml` and `card.md`; the loader refuses it otherwise, so an
  override pack cannot silently disable the card contract.
- A gated stage declares a reviewer, so an auto gate has a decider: its
  reviewer's score. A reviewer or a fold needs a gate, a fold needs a later
  gate to fold into, a loop stage takes no gate, and a pack with a reviewer
  carries `stages/review.md`. No stage id is `review` or `responder` or
  holds a dot, which names a stage's review. The loader refuses a pack that
  breaks one of these, and one whose `gate` is not true or false, such as a
  pack from before version 0.5 with `gate: questions`.
- No include mechanism. Each file stands alone; shared text is duplicated. If
  that hurts, build-time expansion comes later.
- A stage's responder policy comes from the pack; the mode and the
  blueprint's `gate` replies override it (see Interaction).
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
- A non-empty `## Solution` skips the HITL brainstorm.

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
- Type `bug` only for a real defect. Only a bug's title opens with a bug verb:
  the agent routes on the title's first word, whatever the type.
- `skills` names the task skills the work needs.
- `depends_on` within the project only.
- `criteria` names the acceptance criteria the card carries. Every
  criterion of a slice is carried by exactly one card of its project, and
  every manual check of acceptance by a manual entry.

**Body** (plain `##` sections)

| Section             | Content                                                                                                    |
| ------------------- | ---------------------------------------------------------------------------------------------------------- |
| Context             | Why, written into the card. For a bug, the root-cause hypothesis and its evidence.                          |
| Where               | Files, functions and types in this repo, verified at a sha. For wiring changes, every call site.            |
| Solution            | The decided approach. Binding constraints are restated in the criteria, since nothing else reaches review. |
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

**Setup**

- A project without a verify command in CM: the draft proposes one in
  `setup.<project>.verify_command`, and every card of the project runs it
  in its acceptance criteria, written exactly as proposed. The architect
  cannot set it: `create_card` has no verify field, and `update_project`
  keeps the operator's. The sign-off lists setting it as a manual step.
- A repository without `AGENTS.md` or `CLAUDE.md` at the root: the
  project's first card also writes `AGENTS.md` (stack, verify command,
  layout, conventions), `setup.<project>.instruction_card` names it, and
  every other card of the project depends on it. A repository that has an
  instruction file names no such card.

**Target project prerequisites**

- A repository URL on the project. A missing verify command or instruction
  file is setup the cards carry (see Setup), never a block: CM runs a card
  without a verify command, and the agent then detects verification itself.

### Readiness checks

| Check                                                              | How                        |
| ------------------------------------------------------------------ | -------------------------- |
| Required sections present, criteria numbered                       | mechanical                 |
| No local paths, file URLs or another CM project's repository, as CM's self-containment lint finds them | mechanical (lists.yaml and list_projects) |
| No defect phrase or bug title verb in a card that is not a bug; no criteria under a stripped heading | mechanical (lists.yaml) |
| No phrase pointing at text the card does not carry, such as "as noted above" | mechanical (lists.yaml) |
| Every path and symbol in Where exists at the recorded sha          | mechanical (code tool)     |
| `depends_on` graph acyclic within each project                     | mechanical                 |
| Every acceptance criterion maps to exactly one card of its project, every manual check to a manual entry | mechanical (acceptance's artifact) |
| Target project declares a repo                                     | mechanical (`list_projects`) |
| A project without a verify command: the proposed command in every card's criteria | mechanical (`list_projects`) |
| A repository without an instruction file: one card writes `AGENTS.md`, every other card of the project depends on it | mechanical (code tool) |
| Type and priority are ones the project declares                    | mechanical (`list_projects`) |
| One deliverable, no open decisions, no optional scope              | model-judged               |
| Estimated planner split of two subtasks or fewer                   | model-judged               |
| "Could an agent with only this repo finish this card?"             | model-judged, adversarial  |
| No card duplicates an open card of its project                     | model-judged (cm tools)    |

The contract lives here: the default pack's `card.md` and `lists.yaml` plus
`internal/contract`. The agent is the consumer and its prompts are the ground
truth the lists are copied from. The mechanical rows run on every decomposition
submit as the critique loop, against the project config read from CM again
for that submit, so a fix made in CM during the run counts at the next
submit; a submit made while CM cannot be read is refused without spending a
round. The model-judged rows are the readiness stage.

## Interaction

Every stage has one responder policy for its questions and its gate. It
resolves, first match winning, from the blueprint's latest
`gate <stage> human|auto` reply, then the mode (every mode but default
makes it `auto`), then the pack. A stage that declares the `assumptions`
check records what it decides itself as data, and every responder answer is
recorded as an assumption; only these reach the sign-off and the cards.

| Policy  | Questions                                                                                   | Gates                                                                 |
| ------- | ------------------------------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `human` | Asked in rounds in the CM session, one tab per question as `Q<n>` with its options and the recommended one marked. The run ends on the round, which waits as long as it takes, batches of answers included; every answer is recorded in `questions` when the round closes, and the stage resumes from its conversation. There is no limit on a human's questions. | Artifact shown with Approve and Reject; the reviewer's score is shown with it, and its findings in the review line just above; after a revision round the gate shows the revised version and says it was revised, with the version the reviewer scored. The reply, clicked or typed, is `approve` or `reject <feedback>`. |
| `auto`  | A responder run answers each question of the round inside the asking run, which then ends on the round; the stage continues at once with the answers. Each answer is recorded as an `[assumption]` with responder `auto`, and the round is shown read-only. `max_questions` caps the questions per attempt, its builder rounds included; a round past the cap is refused and the stage decides. | The reviewer's score decides: approved at or above the threshold, after one revision round when it lists findings; below it the builder takes another round with the findings, three reviewer rounds per attempt, then a human decides. |

**The responder run** is a separate harness run with the persona of the
stage that asks (the product owner for intake questions, the system designer
for design questions) and `stages/responder.md`. It gets the need, the
asking stage's inputs, the gate feedback the asking attempt addresses, the
user's notes, every answer so far and that stage's read tools, never the
asker's transcript, so the asker cannot answer itself conveniently. It
returns the answer, its reasoning, whether it is grounded and its evidence.
The answer reads verified only when it is grounded and its evidence cites a
source tag that a tool call of that same run returned (tags in the task, the
inputs or its own prose do not count); the checked result is what is
recorded. When it cannot ground the answer it still decides, and the answer
is recorded as an `assumed` assumption keyed by its question number
(`Q<n>`; a verified answer reads `verified`, with the evidence kept in the
`answer` event, so a row stored without it reads `assumed`). That entry is
listed only while no stated assumption rests on the answer (see below). The
sign-off lists every assumption, riskiest first; the export and the playbook
description list every assumption, so whoever reviews the result sees which
decisions the AI made. A responder that fails leaves its question to the
stage, which decides it and records an assumption where it can; under a pack
without `stages/responder.md` an auto round is refused and the stage decides
every question. On a priced model a responder spends
from the asking run's cost cap.

**Assumptions are data.** Before it submits, a stage that declares the
`assumptions` check (intake and solution design) posts its assumptions as
the last tab of a round, labelled A1, A2, ... within the round; a chat that
replaces the tab keeps an unchanged assumption's label and gives a new one
a label the round never issued, so a decision the user made on a dropped
assumption never lands on another. A human
accepts each or turns it into a question: an accepted one is recorded on its
round, whose closed snapshot is the record, and cited as `round<n>.A<k>`, the
round's number and the label (`round2.A1` for A1 on round 2's tab); the stage
states it with that tag as its basis, so it reads `confirmed`. It takes no
question number: `Q<n>` counts only questions a round asked. One a
blueprint holds as a `questions` row stays cited by that row's `Q<n>`, whose
number stays taken. After one is turned into a question, a later round of
the attempt must be posted before the stage may submit. Under the human policy
every stated assumption must have a basis. Under `auto` each assumption is
accepted automatically, logged on the round, and keeps the status its basis
gives it, never `confirmed`; no auto round records an acceptance. The stage
submits each assumption as a standalone sentence, or as
`{"text": "<sentence>", "basis": "<tag>"}` when it rests on an answered
question (`Q<n>`) or an accepted assumption (`round<n>.A<k>`). The check refuses
a sentence that points at other text (the `dangling_references` list) or
names an answered question or an accepted assumption (the basis carries it
and the sentence states the fact, so no model prose says who answered), and
a basis that is neither; the third failing submit is accepted with its
problems kept in its data, and such a basis then reads as none. The controller numbers them
A1, A2, ... across those stages in pack order and gives every later stage
that consumes a stating stage the list with the ids. An assumption starts
with its basis's status: `confirmed` when a human answered the question or
accepted the assumption, `assumed` when the responder answered it,
`verified` with the responder's evidence when that answer was verified, and
`unverified` without a basis. An auto
answer's own `Q<n>` entry is listed only while no stated assumption in the
latest stating artifacts rests on it; otherwise those that do carry its
status, so an answer and its restatement are one entry. Every stated
assumption resting on an answered question carries its answer too, a human's
included, so a merged entry still shows the answer the responder gave. A later
stage (grounding) submits verdicts, verified or contradicted with evidence, as
new artifact data, never an update; a verdict sets the status and evidence. An
unverified verdict, which the controller also accepts, returns the entry to
its basis's status, so it never hides that a human confirmed it. A verdict
counts only when it is newer than the assumptions it judges (the latest
artifacts of the stating stages up to the assumption's own), so a re-run's
assumptions read their basis's status until grounding judges them again. The
sign-off (unverified, contradicted, assumed, confirmed, then verified), the
playbook description and a single card's Context list every assumption with
its latest status; each adds, for an assumed or confirmed one, who answered
the question it rests on and its tag, and the answer, for every entry that
carries one. A confirmed one resting on an accepted assumption reads
"accepted at" the display name of the stage of the tab it was accepted on
(`Intake`) instead, with no tag and no answer. When no stage stated any as data,
as with an intake stored before assumptions were data, they show the text of
intake's Assumptions section instead, without ids or statuses.

Later policy: `jira` (question posted as a comment; after two reminders
without a reply it falls through to `auto`, logged as such).

### Modes

The mode is chosen in the panel's new-session dialog and recorded on the
blueprint.

- **Default**: policies as the pack sets them. The embedded pack makes
  intake, slicing and solution design `human` and every other stage
  `auto`. The sign-off waits for a human. A `gate <stage> human|auto` reply,
  listed in the gate plan posted after intake, changes a stage's policy for
  this blueprint.
- **`auto`**: every policy `auto`. Stops before emission. Cards auto-vet in
  CM on the non-Jira path, so this stop is the only human backstop.
- **`auto+emit`**: as above, then emits the playbook and stops. When it
  approves a clean sign-off itself, it records what it approved, for the sheet.
- **`yolo`**: as `auto+emit`, then starts the run, unless a card's repository
  has no commits: the cards are emitted, the session is told to push a first
  commit and reply `approve`, and no run starts until then. A single card is
  started with `run_card` and ends as one pull request to the project's default
  branch under the card's own settings. Several cards are started with
  `run_playbook`, which makes the playbook runnable and plays it; every card
  then runs with `autonomous`, `create_pr`, `await_ci` and `merge_pr` on, each
  pull request merges into `playbook/<id>` on CI pass, and one compare link
  per repository offers the final pull request to its default branch. Those
  final merges stay human. The session reports the run's progress and end.

The architect's cost cap bounds the architect. The CM runs a yolo blueprint
starts are bounded by CM's per-card guardrails, not by the architect.

## Tools

| Group    | Tools                                                                                                   | Notes                                                                                                                                             |
| -------- | ------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| code     | `repo_info`; harness `read`, `grep`, `glob` jailed to the blueprint's checkout                          | The server keeps one bare mirror per repo (URL from `list_projects`), fetched with go-git under a per-repo lock the first time a run needs the trees: at the start of the first stage with the code tools (intake), or earlier, at a research stage's first leak-guard check that needs them, and one tree per blueprint and repository from that mirror at the default-branch sha it records as a source. Blueprints on the same project never share a tree, so parallel runs never see each other's fetches, and the recorded sha is the one readiness verifies and sign-off compares against. It never reads a developer's own checkout. Credentials come per repo from CM's `GET /api/worker/git-credentials` with the session's token, so project credential bindings apply. A repository with no commits checks out as an empty tree with no sha. `repo_info` returns the sha, instruction file presence, the project's verify command, whether the repository is empty and, to the stage that fetched the tree, the default branch. |
| research | `web_search`, `web_fetch` | Offered to the stages whose pack names the group (intake, slicing and solution design in the default pack) when the session's research settings name a provider the architect can build; otherwise the group is absent and the session says once, at the first such stage, that research is off. `web_search` asks the provider (Brave Search) for ten results: titles, URLs and snippets the model reads in that run only, since a run's stored messages keep only the query and the result count. `web_fetch` reads an http or https page from a public address: every connection's address is checked after the name resolves, redirects included (five at most), and no proxy is used; it reads at most 5 MiB in 20 seconds, keeps 48 KiB of text, turns HTML into readable text (the main or article element, without scripts, navigation or footers), takes text types as they are and refuses the rest. A page read is recorded as a `url` source and its result starts with the tag; a page with no readable text is a tool error, not a source. A leak guard checks every query and every URL's host labels, path and query and refuses, as a tool error naming the rule: a token with a path separator (acronym pairs like CI/CD and HTTP/1.1 pass) or a file extension (a `<name>.js` library passes), code punctuation (a URL path segment may carry a name's `(` or `=`, as in `Raft_(algorithm)`), a compound identifier the blueprint's checkouts declare (not a name of a package they depend on, and nothing in a lockfile), the root module path's owner or name (a nested module's compound name is a declared identifier), a target project's name or its repositories' owner or name, a ContextMatrix card id, and a query, or a URL path segment, query key or query value, over 160 characters; plain words and technology names pass. A URL's host labels get the name, module, identifier and card-id rules but not the shape rules, since a page can ask for a host that carries data; its path segments skip the path and file-name rules, which a URL's own syntax breaks; its query keys and values get every query rule. It reads the checkouts once per run, at the first check that needs them, checking them out first in a stage without the code tools, and refuses everything while it cannot read them. The responder shares its stage's tools. Every query and page read is a `research` event. |
| cm       | `list_projects`, `list_cards`, `get_card`, `get_playbook` as `cm_`-prefixed reads | Offered to grounding and readiness over the session's MCP key, one MCP session per run, chat's bridge adapter on the architect's client with a `cm_` prefix and an allowlist that skips tools CM does not serve. No `agent_id` is sent, so CM keeps redacting unvetted imported text; `get_card` never attaches images; a credential embedded in a URL is removed from every result and error before the model or the record sees it, since CM returns project config as stored; every result carries the blueprint's ContextMatrix source tag. Creating cards and playbooks and starting runs are controller calls with the configured caller identity, never model tools. |
| control  | `ask_questions`, `answer_chat`, `submit_artifact`                                          | `ask_questions` posts a round and ends the run: per question a tab title, the question, why it matters, options with a summary, pros, cons and source tags (only tags a tool result of this blueprint returned; the panel shows a page `web_fetch` read by its title (its address when it has none), linked to its address, and any other source by its identity), and the recommended position; a trailing "(recommended)" a model writes into an option title is cut and marks it. A stage that declares the `assumptions` check may add its assumptions as the round's last tab, or post the tab alone; any other stage's tab is refused. A round asks at most 10 questions of at most 6 options, 96 KiB in all, its assumptions included. A question already answered with the same options is refused; a changed option set makes a new question. Under `auto` its responder answers before the run ends. Questions are numbered `Q<n>` per blueprint. `answer_chat` replies in an open round's chat, at most 4000 characters, revising or adding questions under the same limits or replacing the assumptions tab, and ends the run. `submit_artifact` is terminal: the markdown body and the structured yaml block the stage's schema requires. |

Every tool result passes the harness redactor for every secret the process
holds.

## Emission

Cards that pass readiness go to sign-off. Default and `auto` modes show the
sign-off (the entries in playbook order with the card that also writes
`AGENTS.md` marked, the playbook title, the assumptions and the preflight
result) and wait: `approve` emits, and `reject <feedback>` stores the
feedback and re-runs decomposition and readiness with it, the three
readiness rounds counted again from the rejection. `auto+emit` and `yolo`
approve on their own when preflight runs and finds nothing blocking and no
warning, and stop for a human otherwise. Every decision is a row in `gates`.
Then, in order:

1. **Preflight**, at sign-off and again at the start of every emission
   attempt, reading CM each time. Blocking: a card's project is missing in
   CM or declares no repository. A blocked emission waits for the fix in CM
   and another `approve`. Warnings: a repository whose default branch moved
   since the checkout or could not be read (a move is a changed grounded commit:
   a repository empty at the checkout that has commits now is not a move, the
   push step is done), a project that declares no verify command while the
   cards propose none, or a draft title that is a card in another runnable
   playbook. An `approve` after the warnings were shown is
   the logged override; an emitting mode stops for a human instead. Steps:
   "set verify command `<cmd>` on project `<p>` in ContextMatrix" for each
   project that still declares no verify command, with the command the
   cards propose, and "push an initial commit to `<repo>`'s default branch
   before running the cards; the agent needs a base branch" for each
   repository of a card's project with no commits. Steps are shown at the
   sign-off and again with the emitted cards; they neither block emission
   nor stop an emitting mode, and they are not playbook entries, which would
   hold a run at a check-off. A missing instruction file is never a
   preflight finding: the contract makes a card write it.
2. **Cards**, in playbook order, which the contract keeps dependency-first,
   so `depends_on` carries real ids within a project. Each created id is
   stored in `emissions` at once, so a retry after a failure skips what
   exists. Each body ends with a `## Sources` section saying what every
   source tag it cites stands for: its project's repository at the commit
   the checkout recorded, or the ContextMatrix board. Another project's
   repository is not named, and a body citing no tag gets no section. A
   `create_card` warning is reported and emission continues: CM creates the
   card before it lints it, the readiness checks mirror its lint, and the
   outcomes stage learns from what slips through.
3. **Playbook**, for two or more cards: card and manual entries in draft
   order, the draft's title, and a description with intake's problem and
   value and every assumption with its latest status, with a Sources
   section naming the repositories of every project its cards are in. A
   single card carries the problem and the assumptions with their status
   in its Context section, and its manual checks are listed in the
   session.
4. The blueprint's trees are removed and its state is `emitted`. Running is
   a human action in CM, or in yolo mode `run_card` for one card and
   `run_playbook` for several, started once and recorded in `emissions`.
   While a card's repository has no commits yolo starts nothing and records
   nothing: the session gets a line naming the repositories, saying to push
   an initial commit and reply `approve`, or to start the run in CM and
   reply `approve`. A later `approve` resumes the emission and starts the run
   once every repository has a commit; if one is still empty the line is
   posted again. The watch starts once the run start is recorded.
5. **Watch.** While the session is attached the architect polls CM every
   `watch_interval_seconds` (default 60) and posts a line when a card's
   state, worker, phase or pull request changes, or the playbook run's
   status does. A run waiting on a check-off or on a failed card is reported
   once as waiting for a human. The watch ends when the single card is done
   or not planned, or the playbook run completes or every entry is complete,
   with a closing summary: each card's state and pull request, the playbook
   and one compare link per repository, the architect's total cost, and
   "Reply export for the bundle, or end the session." Then the blueprint's
   state is `done`. The summary is posted once: a restart between the two
   finds it stored and only sets the state, and a reopen of a `done`
   blueprint posts nothing. Ending the session stays with the user.

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
calls and cost stream into it as log entries. The binary,
`contextmatrix-architect`, has one server command, `serve`, plus the
`config defaults` and `config validate` helpers.

Replies the server understands, besides a round's structured answers:

| Reply                         | Effect                                                                 |
| ----------------------------- | ---------------------------------------------------------------------- |
| `approve`, `reject <feedback>` | Decides the open gate. At a stage's gate, approve goes on and reject runs the stage again as a new attempt, given its rejected artifact and every rejection's feedback. At a gate another stage folded into, `reject <stage> <feedback>` runs that stage and everything after it again; a bare `reject <stage>` is a reject whose feedback is the stage's name. At the sign-off, approve emits and resumes an emission that stopped, and reject re-runs decomposition with the feedback. Replies are matched as CM's panel matches a prompt: case ignored, feedback after whitespace, the longest reply winning. With no gate open the text is the need or a note; while a round waits it is chat about the round. |
| `gate <stage> human\|auto`    | Changes a stage's responder policy for this blueprint, for its questions and its gate, from its next round or gate on; an open round or gate keeps its responder. Exactly three words, so an answer that starts with "gate" stays an answer. Works while a stage runs. The sign-off follows the mode and takes none. |
| `export`                      | Writes the markdown bundle (blueprint summary with every gate decision and the emitted ids, artifacts, questions, sources, events, transcripts) under `exports/<blueprint>/<time>` in the server's state directory. The per-project opt-in that commits it to a repository arrives with outcomes in slice 4. |
| `outcomes`                    | Runs the Outcomes stage on the emitted cards.                           |
| `lessons`                     | Lists lessons and their state.                                          |
| `abort`                       | Ends the open run; the blueprint stays and can be reopened, and an open round stays open. |

- A run does not start if it would breach the blueprint's cost cap.
- Reopening a session makes CM send a start for it; a stage interrupted by a
  server restart is re-run from its inputs, or from its stored conversation
  when it went on from a round (below), and a stage whose inputs were stored
  but which never started runs. Completed artifacts are never lost. A
  record the pack cannot place, such as a stage the pack no longer has, is
  reported in the session and nothing runs. An open gate or sign-off is shown
  again as it was, a gate that was never shown opens under the policy now in
  effect, a review or builder round, a revision round included, a restart or
  the session's end cut short runs again, one a crash kept from starting
  runs, an emission a restart cut short resumes, a stopped one waits for
  approve, an emitted blueprint's watch restarts, and nothing is created or
  started twice. CM sends the start again after the architect restarts, when
  a message finds the session unknown or the log stream ends.
- A stage the session's end cut short runs again on reopen, given the answers
  its earlier runs got and who gave each; a reopen that comes while that run
  still winds down waits for it first. A round outlives End, a restart and
  days away: its stored answers stay, a reopen shows it again with them, and
  it waits; a chat message that a chat run cut short by the session's end
  or a restart left unanswered is answered first. A continuation that the
  session's end or a restart cut short runs again on reopen from the same
  stored conversation, and a round that
  closed but never continued continues; a close cut short finishes, and no
  answer is recorded twice. An abort ends a run in flight, never a round. A
  round the pipeline went past without its answers, as when the run that
  posted it never recorded its end, is collapsed in the panel, and its stage
  runs again as any stage cut short does. An aborted stage waits for the
  next message, as below.
- After a later stage fails or is aborted, the next message is logged as a
  note and re-runs that stage; after a continuation, one of intake included,
  fails or is aborted, the next message is a note and runs it again from the
  same conversation; after a review or a builder round, a revision round
  included, fails or is aborted, the next message is a note and runs it
  again; after intake fails or is aborted, the next message is the need
  again and intake re-runs on it. A pipeline stopped by the card contract or
  by readiness's rounds is not re-run; the user starts a new blueprint with
  a better design.
- From slice 4, a project may opt into automatic export in config: a target
  repo, branch and directory; the bundle is committed there at each gate or at
  sign-off. Off by default.
- A message that arrives after the need, while no human's round waits
  (then it is chat about the round), and that decides no open gate is a
  note: it is logged and given to every model run that starts later, in
  arrival order. A note never reaches a run already in
  flight, and a note at an open gate leaves the gate open. A note that is only
  `approve` or `reject` is a misplaced decision, not guidance: it is logged
  and reaches no run.

## Blueprint view

CM's pane for a blueprint session shows where the blueprint stands beside
the transcript: the pipeline as a scale bar, the cost against the cap, and
every artifact on a sheet. The architect owns that data. CM proxies the
two reads, the view and an artifact, for architect sessions only and checks
the session's owner, and relays the live stream: it holds one upstream
connection per session while browsers are subscribed and publishes the
frames on the session's chat stream.

- **View.** `View` builds it from the store and what is in flight: one step
  per pack stage, plus `signoff` and `emission` when the pack checks cards.
  Each step carries its persona, gate, policy, fold target, status
  (pending, running, reviewing, revising, waiting, done, failed, folded or
  skipped), attempt, a loop stage's round of its maximum, latest version
  and cost. The view also carries the run in flight (stage, kind, persona,
  model, start, and the turns, tool calls and tokens its harness lines have
  counted), the open prompt (`round:<n>` of kind `round`,
  `gate:<stage>:v<n>`, `gate:signoff:v<n>`, or `emission:v<n>` for a
  stopped emission or run start), and the cost: the ledger plus the runs in
  flight, the blueprint and per-run caps, and whether every run was priced.
  While a round waits, its stage's step is waiting and no run is in
  flight; while a chat run answers its chat, the step is running and the
  round stays the open prompt. Every artifact version is listed with its
  status (current, reviewing, at_gate, approved, rejected, superseded,
  folded), who decided
  its gate (you, the responder, or automatically for a sign-off an emitting
  mode approved) and when, and its reviewer's verdict. A rejected version
  stays rejected; any other is superseded once a newer one exists, or once
  a gate row records it superseded, as `reject slicing` does for the design
  version it replaces. The newest version reads reviewing while its review,
  or the builder round after it, is owed, and current once that run fails,
  as its step then reads failed. A verdict's findings are sent to the next
  version when the verdict sends the builder back. A finding is marked
  addressed in that version, with its note, only when the revision listed
  it. A run whose end is recorded counts in the ledger only.
  `Seq` is the id of the newest events row the view reflects, and every
  state change writes one. The view reads the store and memory only, so an
  ended session's blueprint has one while the architect is up. A record the
  pack cannot place shows every step pending.
- **Artifacts.** `Artifact` returns one version's body, data and the
  problems its checks accepted it with. The sign-off is a pseudo-artifact
  per version recorded, its data the sign-off's payload. An emitting mode
  that approves a clean sign-off on its own records what it approved (a
  `signoff_auto` event after the approval, never shown in the transcript),
  so its artifact exists, stamped approved automatically. The board
  (`emission`, version 1) exists once the cards are emitted. Its data is
  the created cards with their ids and, once the watch has seen them, their
  state and pull request, plus the playbook, compare links, manual checks
  and steps, and the total cost. The need as typed and reviewer verdicts
  are not artifacts here; verdicts travel in the view.
- **Live stream.** `Subscribe` sends a snapshot of the view first. After
  every controller event, state change and harness line it sends a fresh
  snapshot, at most four a second. It sends an `artifact_stored` hint when
  a stage's artifact, a sign-off or the board is stored or changes, and
  each frame of the draft being written. Draft offsets count code points:
  offset 0 replaces, an offset equal to what the client holds appends, and
  any other is a gap the client drops until the next snapshot, which
  carries the whole draft. A snapshot without a draft means none is in
  progress: a stage run's start or end, a stored artifact and a restart end
  it, and within a run a refused draft stays until the next attempt
  replaces it. Each subscriber's queue is bounded. One that falls behind
  loses its queued drafts and gets a snapshot next. Publishing never waits
  for a subscriber.
- **Endpoints.** Signed `GET /blueprints/{id}`,
  `GET /blueprints/{id}/artifacts/{stage}/{version}` and SSE
  `GET /blueprints/{id}/live`, behind the same HMAC auth as `/logs` and
  outside the drain gate. They ignore unknown query parameters: CM signs a
  nonce into each read, so two reads of one URI never replay. A stage is
  `[A-Za-z0-9_-]{1,64}`. The live stream opens with `: connected`, sends one
  `data:` line per frame and a keepalive comment every 15 s, and ends at
  drain. CM proxies the two reads for architect sessions only, checking the
  session's owner, and relays the live stream: one upstream connection per
  session while browsers are subscribed, its frames published on the
  session's chat stream.
- **Drafts.** While a stage run's model writes `submit_artifact`, the
  harness passes each argument fragment on. The stage runner decodes the
  top-level `body` string as it grows (escapes, `\u` and multi-byte runes
  split across fragments) and redacts the whole text with the session's
  secrets, holding back a tail that could be the start of a secret. It
  publishes at most four frames a second, the whole body when the submit
  arrives, and a refused frame with the check's error when a check refuses
  it. A reset before a replay after a mid-stream retry, and the next
  attempt after a refusal, restart at offset 0. A harness error or a
  compaction discard voids the draft: the harness abandoned that turn and
  sends no reset for it. A submit whose arguments the harness repairs never
  executes, so its draft is marked refused with the repair error. Each model
  call starts a fresh draft, whatever tool-call ids the provider reuses.
  Reviewer and responder runs draft nothing. A model whose stream carries
  no tool-call fragments shows its body at submit. A restart loses the
  draft, and the run re-runs from the ledger.

## Deployment

- **One server.** `contextmatrix-architect serve` runs next to the agent and
  chat backends: in a container, or as a systemd user service through
  `svc.sh`. It is configured in CM as a backend of kind `architect` with its
  URL and an HMAC key, exactly like the other two.
- **Provisioned per session by CM.** The start payload carries the LLM
  endpoint, the MCP key, the model, the git-credentials token and, with
  research on, the search provider and key (`ArchitectStart.Research`; none
  means research is off). The architect holds none of these in config or
  env, and refuses a start without the endpoint, as the chat backend does.
- **Code.** The server checks out each target project's repo from the URL CM
  reports, into its own workspace cache, with credentials fetched per repo from
  CM. Nothing is checked out on a client.
- **Container needs.** A Go binary image with ripgrep on the path (the
  harness grep and glob tools exec it), no Docker socket, a volume for the
  state directory (the image sets `XDG_STATE_HOME=/var/lib`, so the default
  state directory is that volume), and outbound access to CM, the LLM endpoint,
  the git host and, with research on, the search provider and the public web;
  `web_fetch` dials pages directly and never uses an egress proxy, since its
  address check must see the page's own address. Env: the CM base URL and the
  backend HMAC key.
- **Host needs.** As a user service the same binary needs ripgrep on the
  service's PATH. `state_dir` defaults to the XDG state directory, which
  `svc.sh` whitelists and creates. `serve` logs a
  warning at startup when rg is missing and starts anyway.
- **Client.** The panel, through CM. Local use is
  `contextmatrix-architect serve` on the laptop pointed at a CM instance.
  There is no CM-less mode.

## Runtime

- Go 1.27, `contextmatrix-harness` v0.26.0 (loop, llm client, jailed read
  tools, Skill tool, redact, events), `contextmatrix-backendkit` v0.9.0
  (`webhookcore`, `logbridge`, `metrics`) and `contextmatrix-protocol` v0.27.0
  for the CM-facing side, go-git v5.19.2 for mirrors and tree export, `modernc.org/sqlite`, the Go
  MCP SDK as a client, cobra and koanf as in the agent.
- Server config file plus `CONTEXTMATRIX_ARCHITECT_*` env overrides: CM base
  URL, backend HMAC key, listen address, state directory, pack, the caller
  identity for ContextMatrix writes (default human:architect), the watch
  interval, per-project export opt-ins (slice 4), and optional per-persona
  model overrides (slugs only).
  Secrets reach only their client and never a log, error, artifact or store
  row.
- Models: the start payload's model is the default for every persona; config
  may override per persona. Priors-based selection through
  `contextmatrix-protocol`'s `selection` package comes later.
- Tool results are capped at 128 KiB per call before the model sees them,
  head and tail kept, as in the chat backend.
- Cost: every run's token buckets are priced at the model price CM sends in
  the start payload; the ledger sums runs per blueprint. The per-run cap
  (off at zero) stops a run within one model call of passing it, on a priced
  model a responder run spends from the asking run's cap, and a capped
  blueprint whose model has no price does not run. An unpriced model on an
  uncapped blueprint counts only the cost the endpoint reports, and
  max_turns still bounds every run. CM prices only the session model, so a
  run on a per-persona override is unpriced: a capped blueprint refuses it,
  an uncapped one counts the reported cost. A critique round re-bills the
  conversation so far, and so does a revision round; the ledger counts both.
- Every stage run compacts in its window: once a prompt reaches 85% of a
  128000-token window, the harness summarizes the older turns, keeps the
  last six messages and goes on. A model with a smaller window fails on the
  provider's error first.
- State dir: `architect.db`, `exports/<blueprint>/<time>/`,
  `workspaces/mirrors/<repo>/` and
  `workspaces/trees/<blueprint>/<project>/`, with a project's secondary
  repository beside it at `<project>@<repo>/`. A blueprint's trees are
  removed at emission and by a sweep, at startup and hourly, for blueprints
  not written for `workspace_max_age_days`; mirrors stay. Trees are written
  from the mirror's objects; the server never runs git. `workspaces/` is a
  disposable cache; `exports/` is the operator's record and is never swept.
- LLM endpoint: whatever CM provisions, OpenRouter or any OpenAI-compatible
  endpoint. Never name a gateway operator in repo artifacts.
- Builds on macOS too: no filenames differing only by case.

## Package layout

```
cmd/contextmatrix-architect
internal/blueprint   state machine, gates, reviewer rounds, responder policy, cost cap; the command and event interface
internal/pack        pack loading, overlays, lists
internal/stage       stage runner: prompt assembly, tool sets, output schema, critique loop
internal/contract    card contract checks, mechanical and model-judged
internal/store       SQLite: schema, append-only tables, queries
internal/code        workspace checkout and repo_info
internal/research    web_search and web_fetch, the leak guard, the public-only fetcher, the search provider
internal/cm          MCP client: the project list, the stage read tools, and the card, playbook and run calls emission makes
internal/outcomes    outcomes stage and lessons
internal/export      markdown rendering into the state directory
internal/config
internal/webhook     CM-facing chat-backend protocol on backendkit webhookcore and logbridge; routes the abort, export and gate commands that internal/blueprint recognises; serves the blueprint view, artifacts and live stream
```

## Changes needed in CM

| Change                                                                                             | Needed for          |
| -------------------------------------------------------------------------------------------------- | ------------------- |
| `run_playbook` MCP tool: makes the playbook runnable if it is not, then plays it; `human:` caller gate like `run_card` | yolo with several cards (slice 2); a single card uses the existing `run_card` |
| An `architect` backend kind: config entry with URL and key, its own callback prefix, session start dispatched by kind in the chat manager with the existing `ChatStartPayload` | slice 0 |
| Chat sessions carry a kind and a blueprint binding instead of only a project; a one-way ops.db schema bump | slice 0 |
| Panel: a new-session dialog for blueprints with a multi-project picker, mode and cost cap | slice 0 |
| The git-credentials endpoint treats an attached architect session as live | slice 1 |
| CM re-sends the start to an architect session after the architect restarts: on a 404 `not_found` for a message (then re-sends the message once), and when the log stream ends | slice 2 |
| Panel: question and gate prompts (`LogEntry.Prompt`, protocol v0.24.0, with their own text, kind and recommended option from v0.25.0) rendered as buttons; the gate plan stays text, and lesson prompts reuse the same shape | slice 3 (lessons slice 4) |
| Panel: the blueprint pane (scale bar, sheet, transcript markers), read through CM proxies of the architect's signed view and artifact endpoints, with its live stream relayed on the session's chat stream (protocol v0.26.0) | blueprint UI |
| A research setting (`off` or `search`, off by default) with the search provider and its key in CM's configuration, sent as `ArchitectStart.Research` (protocol v0.27.0) | research |
| Panel: a gate inbox | later |
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
4. **Outcomes and lessons**, with pack overlays, and the per-project export
   opt-in that commits the bundle to a repository through an allowlist of
   target repositories.
5. **Later**: a gate inbox in CM's web UI;
   seeding a blueprint from an existing CM card, in the same adapter shape as
   the Jira seed; Jira seed and mirror (decide REST against Atlassian MCP then,
   see open questions); composer; model selection via
   `selection`.

## Testing

- **LLM**: a scripted OpenAI-compatible `httptest` server behind the harness
  client, as the agent does. No live model in `make test`.
- **Contract checks**: pure functions, table-driven over fixture cards, good
  and bad, including the routing lists.
- **Store**: temp SQLite. Tests prove no update or delete path exists for
  append-only tables.
- **Controller**: driven through its command and event interface, table-driven
  state transitions. A round test posts a round, answers it through the
  interface in two batches and asserts the stage resumes its conversation
  with the answers.
- **Critique loop**: a submit that fails a mechanical check is returned to
  the model as the tool result, and the third failing submit is accepted,
  stored and reported.
- **Emission**: an in-test MCP server asserting call order, and that a re-run
  after a simulated partial failure creates nothing twice.
- **Webhook**: an in-test CM that signs requests starts a session with a
  payload, sends a round reply, sends each reply form and asserts its effect, and reads the SSE log entries; an
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
- Data handling: code and ticket content go through the LLM endpoint. Personal
  data in tickets needs a policy before the Jira slice.
