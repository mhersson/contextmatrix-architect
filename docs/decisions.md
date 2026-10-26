# Decisions

The decisions the code keeps, each with its reason; every entry is true at
HEAD.

## Boundaries

- After emission ContextMatrix is the source of truth for the cards, and
  corrections are made there: the architect never edits a card it created,
  and its only later calls on one are reads and the run start, whose
  run_card or run_playbook sets the run flags. Why: the architect's record is
  the plan, not the board.
- The core is methodology-neutral: BDD, Gherkin, sizing schemes and
  compliance lists belong in a pack, and Given/When/Then in a card body is
  acceptance-criteria text. Why: the core encodes no one team's process.
- A session start without an LLM endpoint is refused, and there is no local
  fallback. Why: ContextMatrix provisions everything per session, so the
  architect fails closed.

## Architecture

- A standalone Go service on contextmatrix-harness, with ContextMatrix kept a
  coordination layer. Why: the chat backend cannot host stage control or
  gates: its resume is lossy, and it has no terminal tools and no responder
  policy.
- Not built on contextmatrix-agent; contextmatrix-backendkit is used only for
  the ContextMatrix-facing side, as webhookcore and logbridge. Why: the agent
  is shaped for card work in a container, and nothing under its `internal/`
  can be imported.
- One `serve` process owns the store, the workspaces and every harness run,
  and every session starts in ContextMatrix. Why: one record and one
  checkout location, with no laptop copy of anything.
- A pack's stage list is fixed and repeats its per-slice stages once per
  slice; no composer picks stages for a need, no size is estimated up front,
  and a stage with nothing to do collapses. Why: every stage earns its place
  for any need, and how much runs falls out of the artifacts' counts.
- Personas are prompt text on stages, not an agent system. Why: a
  composer-and-agents layer would be a second decision engine that has not
  earned its place.
- No memory or knowledge layer: retrieval is live, and each blueprint records
  the sources it used with their kind and version. Why: live retrieval with
  versioned sources needs no layer, and one could be fed only approved
  outputs.
- No bespoke web UI: ContextMatrix's chat panel renders the sessions, and
  richer controls are ContextMatrix web changes. Why: the panel brings the
  session list, auth and look for free.
- ContextMatrix only proxies the signed view and artifact reads for the
  architect sessions a caller owns, and relays the live stream; the architect
  owns the data. Why: one source for the blueprint view.
- Every run of a stage, a chat run included, is offered the same tool list,
  and one reasoning effort is sent on every request. Why: a resumed
  conversation keeps its prompt cache on either endpoint.
- A button click sends its reply as an ordinary message, and a round or
  mockup reply's content is never parsed as a command. Why: a click and a
  typed reply parse the same way, and a structured reply's content is only a
  readable summary of its payload.
- Text a person reads names a stage by its display name; prompt ids, event
  payloads, markers and the `gate <stage>` command keep the raw id. Why:
  commands and the record stay stable.
- No stage is named "architecture" (that work is solution design), no name
  is a film reference, and big-brain, Keymaker, Basis, Pivot, Factor, Span,
  Foreman, orchestrator, researcher and explorer are not used as names. Why:
  "architect" always means this tool.

## Data and record

- SQLite is the record, and the markdown export is a rendering of it written
  to the state directory. Why: committing per-stage markdown into code
  repositories bloats them.
- Only artifacts carry forward between stages, never transcripts; another
  slice's artifact is given as a digest, and `read_artifact` returns it
  whole. Why: each step reads decided output, not the reasoning behind it.
- A step key is `<stage>@SL<n>`, and no stage id holds `@` or `.`
  (`<stage>.review` names a reviewer's verdicts). Why: slice runs and
  verdicts keep records of their own.
- Schema upgrades are one-way: an older binary refuses a newer store. Why:
  there is no downgrade path.
- Assumptions are data: tab labels are per round and never reused, an
  accepted one is cited `round<n>.A<k>` and takes no `Q` number, a later step
  never restates one, and a grounding verdict counts only when it is newer
  than the assumptions it judges; one on an auto answer's `Q<n>` also reaches
  an entry stated later that rests on it, unless a verdict at least as new
  judges that entry's own id. Why: a decision never lands on the wrong
  assumption, and a re-run never hides behind an old verdict.
- A responder answer reads "verified" only when its evidence cites a tag that
  a tool call of the same run returned. Why: tags in the task or in the
  responder's own prose prove nothing.
- The server never runs git: trees are written from mirror objects. Why: no
  git binary is needed, and nothing writes to a repository.
- `workspaces/` in the state directory is a disposable cache, and `exports/`
  is the operator's record, never swept. Why: the exports are the audit copy.
- Each blueprint gets its own tree per repository, written from a shared bare
  mirror, and a developer's checkout is never read. Why: parallel blueprints
  never see each other's fetches, and the sha the checkout recorded is the
  one readiness reads and the sign-off's preflight compares.

## Stages and gates

- Every stage of the default pack plans only what the need asks: each part an
  artifact adds traces to a need item, an answer, an accepted assumption,
  gate feedback or a defect it prevents in existing code; the working whole
  is in scope, and doing the work correctly and securely on real input is the
  work itself. Why: no gold-plating, and no hollow result.
- A check's refusal goes back to the model, and the third failing submit is
  accepted with its problems kept, except a fatal problem (the slices check's
  shape and no_slices; the decisions check's shape, no_slices and
  slice_missing; the criteria check's shape, no_slices, slice_missing and
  slice_empty), which fails the run with `fatal_check`. Why: such an artifact
  leaves the next step nothing to build on.
- Each gated artifact is reviewed from the seat of the role that works from
  it next; a finding counts as fixed only when the reviewer sees it fixed,
  and an open critical finding fails the review whatever the score. Why: a
  reviewer that trusts the builder's notes passes artifacts the next role
  cannot use.
- No version reaches a gate unreviewed, and the reviewer never gets the
  builder's transcript. Why: the review is an independent check.
- The responder never sees the asker's transcript. Why: an asker cannot
  answer its own questions through the responder.
- The pack loader refuses a cards stage without the code tools, lists.yaml
  and card.md, and a gated stage without a reviewer. Why: an override pack
  cannot silently disable the card contract, and an auto gate needs a
  decider.
- No include mechanism: each pack file stands alone, and shared text is
  duplicated. Why: simplicity until it hurts.
- A shared stage collapses with fewer than two slices, recorded as a fold at
  version 0 into the first slice's design gate, and slicing's gate folds
  there too. Why: a one-slice need still has one design gate.
- Folding is decided from the artifact, never from a size estimate, and is
  recorded as a decision. Why: a later policy change cannot unfold it.
- Readiness judges every slice's cards as one draft, and a not-ready verdict
  re-runs the earliest slice holding a failing card and every later slice; it
  is one step for the blueprint, so its rounds count across slices, and as
  decomposition's reviewer it has none of its own. Why: later slices' cards
  build on earlier ones.
- The first stage with the code tools reports what each target project lacks
  for an agent run (a verify command, an instruction file, any commit), and
  none of it blocks. Why: decomposition plans it as setup.
- The proof-of-concept flag changes what cards demand, never the design; it
  is read fresh for each run that writes or judges cards, and a failed read
  fails that run. Why: a guess would either lower a production bar or spend
  a sweep the operator turned off.
- On a plain-language blueprint every stage gate is auto unless a `gate`
  reply says otherwise. Why: approving a design is a technical decision.
- No extra model run guards the plain wording: the stage reviewers judge it
  in their own runs, and the server's plain texts are fixed per situation.
  Why: no added cost, and predictable wording.

## Card contract

- A card is one deliverable sized for one pull request and at most two
  planner subtasks, with an acceptance-criteria section and a project verify
  command. Why: across 341 and 161 agent-run cards on two instances,
  struggle rose from 6% to 26% at two or more subtasks, and from 8% to 25%
  when the criteria or the project verify command was missing.
- The agent facts the contract rests on (stripped headings, routing phrases
  and verbs, maintenance labels) are data in lists.yaml, re-checked on every
  agent release. Why: the agent's prompts are the ground truth.
- Only a bug card's title opens with a bug verb, and a card that is not a bug
  carries no defect phrase, even negated. Why: the agent routes on
  substrings and on the title's first word, whatever the type.
- A card carries no heading the agent strips, `## Diagnosis` and `## Design`
  among them. Why: the agent discards a written Diagnosis, and Design never
  reaches the coder or the reviewer.
- A contract between two cards is written into both, word for word. Why: each
  agent run sees only its own card.
- The architect never sets a project's verify command; a proposed one is a
  setup step at the sign-off, or, for a project that declares none, a notice
  when yolo starts the run at once.
  Why: create_card has no verify field, and the project's command stays the
  operator's (the architect never calls update_project).
- A proposal replaces a declared verify command only through a `replaces`
  that names that command exactly. Why: a person's own command stands.
- A worker is a container with the checkout, its toolchain and network for
  packages, and no database, Docker daemon, cluster or deployed environment;
  suites that need a service go to the pull request's CI, and CI
  configuration is a source, never proof. Why: a verify command runs only
  what a worker can run.
- A missing verify command or instruction file is setup the cards carry,
  never a block. Why: ContextMatrix runs a card without a verify command, and
  the agent then detects verification itself.
- A repository that does not ignore what its stack writes gets the
  `.gitignore` change as its first card's first change. Why: the card's first
  commit then carries it, before anything is built.
- The mechanical checks run on every decomposition submit against project
  config read from ContextMatrix again, and a submit made while ContextMatrix
  is unreadable spends no round. Why: a fix made in ContextMatrix counts at
  the next submit.
- A per-slice submit is judged together with the earlier slices' drafts,
  counting only the findings it brought. Why: depends_on and setup reach
  earlier slices.

## Mockups

- Where a UI exists the mockup keeps its look unless intake's data holds
  `redesign: true`, and an intake without the flag reads as no re-design.
  Why: a change to an existing UI fits the look people already use.
- ContextMatrix's sealed frame is the boundary, and show_mockup's source
  check is only a quality guard. Why: the check keeps a revision from showing
  broken; the frame is what contains it.
- No mockup source line, trimmed, starts with `## `, and a screen's or a
  suggestion's title and an option's label are one line not starting with
  `#`. Why: the agent's section parser trims lines and does not track fences.
- The mockup block grammar is shared with ContextMatrix's lint and the
  agent's classifier; the phrase scans skip blocks, the structural scans do
  not, and a draft card body holds no block. Why: every block the scans skip
  is one emission wrote.
- Every card of a slice that builds a screen carries the full baked source,
  and no repository file or depends_on carries it. Why: the work behind a
  screen sees the target it serves.
- The auto responder makes at most 5 change requests per mockup. Why: it
  bounds a mockup's cost.

## Emission

- Two or more cards go out as a playbook; one card goes out bare and runs
  with run_card. Why: a one-card playbook would end in two pull requests, one
  to the playbook branch and one to the default branch.
- Preflight's setup steps and notices are never playbook entries. Why: an
  entry would hold the run at a check-off.
- A missing instruction file is never a preflight finding. Why: the contract
  makes a card write it.
- A create_card warning is reported and emission goes on. Why: ContextMatrix
  creates before it lints, readiness mirrors the lint, and the retrospective
  learns from what slips through.
- `followups` is left out of create_playbook when there are none. Why: an
  older ContextMatrix refuses the key.
- The manual entries after the playbook's last card, in the last slice's
  draft only, move to follow-ups mechanically; any other follow-up is one the
  draft lists itself, where its instructions allow. Why: ContextMatrix's
  runner stops at every manual entry in order, and telling a real gate from a
  check is judgement that lives in the prompt.
- The watch takes a card or the playbook for deleted only on ContextMatrix's
  own answer: a playbook entry flagged missing, or a read refused as not
  found. Why: a failed read, a timeout or ContextMatrix being down must never
  end a watch.
- Cards are never created through ContextMatrix's Jira import. Why: imported
  titles start with the issue key, no labels are set, and the epic-context
  section is not stripped.

## Modes

- In auto mode the sign-off is the only human backstop. Why: cards that do not
  come from a Jira import are vetted in ContextMatrix by default.
- In yolo a playbook's pull requests merge into its playbook branch, and the
  final merge into each default branch stays a person's, offered as a compare
  link. Why: a person reviews the whole change before it reaches main.

## Learning

- Lessons take effect only after a person approves them. Why: a blueprint
  yields a handful of cards, so an early lesson can be noise.
- A blueprint's lesson set is fixed at creation, from the active lessons for
  its pack and projects, and a reopen never changes it. Why: a blueprint's
  checks stay stable.
- Lessons reach only the cards and readiness stages, and their checks are not
  project-aware. Why: lessons are about card writing.
- A lesson extends the lists only as an overlay merged after the pack's
  entries and checked by the loader's parser. Why: a lesson never needs a
  code change.
- The retrospective reads cards with get_card only. Why: it needs no
  ContextMatrix change.

## Research

- Search result lists are never stored, and there is no switch to store
  them; of web research, only the pages web_fetch reads are sources. Why: the
  search provider's terms forbid storing results, and no storage rights are
  bought.
- web_fetch dials directly, never through a proxy. Why: its public-address
  check must see the page's own address.
- web_search and web_fetch trust the system store alone, never
  `ca_cert_file`, which the LLM, ContextMatrix and git clients add to it.
  Why: research reaches public hosts only.
- The leak guard refuses paths, file names, code punctuation, declared
  identifiers, module, project and repository names, card ids, and more than
  160 characters, in queries and URL parts; plain words and technology names
  pass. Why: code and project names never leave the installation.

## Security

- Card and playbook writes and run starts are controller calls made with the
  caller identity, never model tools. Why: the identity that passes
  ContextMatrix's human gates must never reach the model.
- ContextMatrix reads strip `agent_id` and never attach images. Why:
  ContextMatrix then treats the caller as an agent and keeps redacting
  unvetted imported text.
- Credentials embedded in URLs are scrubbed from every result and error of a
  stage's ContextMatrix read tools, and repository URLs are used and recorded
  without their userinfo. Why: ContextMatrix returns project config as
  stored, and the record is append-only.
- Every tool result passes the redactor for every secret the session holds
  (the LLM key, MCP key, git token and search key), and live drafts are
  redacted with a held-back tail. Why: a secret never reaches a
  model-visible or stored text.

## Runtime and build

- Reasoning is replayed only to the model that wrote it, and a 400 on a
  request carrying reasoning is retried once without it. Why: another model
  cannot validate it.
- Tool results are capped at 128 KiB with head and tail kept, and compaction
  runs at 85% of the window keeping six messages, both as in the chat
  backend. Why: a ContextMatrix card read can carry a long activity log.
- No two file names differ only by case. Why: the build works on macOS's
  case-insensitive filesystem.
- `make test` never calls a live model, and store tests prove the
  append-only tables refuse an update or a delete. Why: tests stay
  deterministic, and the append-only invariant has proof.
