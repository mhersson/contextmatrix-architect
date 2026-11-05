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
- No memory or knowledge store of its own: retrieval is live, from the code
  and the packages its styles import, the board, web pages and, through
  ContextMatrix, a project's knowledge provider, and each blueprint records
  the sources it used with their kind and, where one exists, version. Why:
  live retrieval with recorded sources needs no layer, and one could be fed
  only approved outputs.
- The knowledge tools are offered wherever ContextMatrix serves them, and a
  knowledge session that fails to open leaves the run without them. Why:
  knowledge never fails a blueprint, and a ContextMatrix that serves none (an
  older one, or one with no provider configured) reads as before.
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

- A knowledge result is a source only when it returns items, one source per
  provider per blueprint, and a citation names the item, as `[S4 R-12]`. Why:
  an empty answer has nothing to cite, and an item carries no version a read
  could cite.
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
- Only the knowledge item ids a knowledge read of the responder's own run
  returned count, and the sign-off names those with the assumption. Why: an
  id the run never read proves nothing, and one that run did not get back
  would be a false claim at the gate.
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
  gate feedback, a defect it prevents in existing code or an agreed rule of
  the product's knowledge that the asked-for work must keep; the working whole
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
- Knowledge adds no human stop and no sign-off warning: a card that goes
  against an agreed rule is an ordinary readiness card finding inside
  readiness's rounds, and a draft rule or open decision is asked like any
  other question. Where the responder answers (Auto, Auto + emit, Yolo, or a
  stage whose policy is auto) it decides one as an assumption naming the
  item. Why: knowledge is a guardrail for automation, used most in Auto,
  Auto + emit and Yolo.
- Intake reads the product's knowledge for the need whenever it has the
  tools; every other knowledge instruction in the default pack applies only
  when the tools return items. Why: without a provider each stage works as
  before once Intake's first read comes back empty.

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
- A stage with the `design` key, the default pack's mockup, gets the target
  projects' design material gathered by the controller before each run, as a
  brief that opens the task after any lead the stage's cm, knowledge or
  research tools add; the pack loader refuses the key without the code
  tools. Why: the brief points at files read with those tools, and no model
  picks a download.
- Frame files are found by name. A file named for a frame word (layout,
  shell, header, navigation bar, footer, sidebar, hero and the like) counts
  when it is a `.tsx`, `.jsx`, `.vue`, `.svelte`, `.html` or `.css` file in
  any folder; a `.ts` or `.js` one counts only in a component or layout
  folder or as its folder's own file (`Navbar/index.ts`,
  `header/header.component.ts`), a folder's own file counted as its folder.
  So do layout files anywhere, any file of a layout folder and the app shell,
  such as a file named `App` at the root or one directory below (`app.vue`,
  `src/App.tsx`, `src/app/app.component.html`). Files under api, constants,
  utils, hooks, pages, services, store, types, stories, icon, context,
  provider, test, email or mail folders, hooks (`use...`) and files named for
  those folders (`types.ts` beside a layout) never count. They are ranked by
  depth, frame word and path, at most 20. Known limit: a frame component with
  an unusual name is missed, and the design-system components in the frame
  source are translated to HTML by the designer from the kept CSS. Why: the
  designer drew a made-up frame when noise outranked the real components, and
  flat, Vue, Angular, Next app-router, SvelteKit, Feature-Sliced and Django
  apps keep their frame files outside component folders.
- The brief carries the text of the top frame files and of the first SVG
  logo outside its 16 KiB cap, within a quarter of the run's model window
  (the default window when it is unknown), at most 128 KiB: frame files take
  three quarters of it and logos one, each shared among the roots by need,
  so a root that needs little leaves the rest to the others, up to 32 KiB of
  frame files and 16 KiB of logo per root. Each file is counted with its
  label and fence; a file over budget, binary or not UTF-8 is listed by path,
  and a root none of whose text fits gets no frame source. Why: told where
  the frame was, designers on more than one model still invented it, and the
  task must still fit a small window.
- Repository text in the frame source sits in a backtick fence longer than any
  run of backticks in it and tagged by the file's extension, every line break
  is `\n`, and a line that, trimmed, starts with one or more `#` and then
  white space or its end has its first `#` written as `&#35;`. Why: the text
  lands in the architect's own task prompt, whose sections are `## ` headings,
  so repository text must not open one, and the designer copies frame text
  into a mockup source that cards carry.
- The only packages downloaded are those the project's own CSS imports (a CSS
  `@import`, or a JavaScript side-effect import of a package's `.css` file)
  and the importing frontend's package.json declares, at the version the
  nearest lockfile (package-lock.json at lockfileVersion 2 or 3, yarn.lock or
  pnpm-lock.yaml) at or above that frontend, up to the repository root,
  records; the same rules apply to the packages a kept package's CSS imports,
  checked against that package's own package.json and its lock entry. Only a
  registry package is fetched, never a git or file entry. A workspace package
  (an npm link, a pnpm `link:` or `workspace:` dependency, a Yarn Berry
  `workspace:` one) the root imports by name is found in the tree by its
  package.json `name`, and the file its exports or the plain subpath names is
  read like a relative import, its package imports checked against its own
  package.json and lock entry; no package of that name, or more than one, is
  a gap. Why: the architect gets exactly what the project installs, and pnpm
  records no workspace path, so one rule by name serves every lockfile.
- Package downloads are anonymous: no credential setting is read from any
  config and no Authorization header is sent, a lockfile url that is used is
  requested as written, query included, and an address carrying a username is
  refused. Why: the architect's secrets are its HMAC key and what
  ContextMatrix sends, and a repository's token is neither.
- The registry comes only from the repository's own `.yarnrc.yml`, `.npmrc`
  or `.yarnrc`, else the public npm registry; a registry value using an
  environment variable is not used, and a lockfile's tarball url only when it
  is https on the registry's host. A lockfile url on another host gives way
  to the registry's own url only when the download is checked against the
  lockfile's integrity or the url has the registry's tarball path; otherwise
  the package is a gap. Why: the machine's config and environment are not the
  project's, a lockfile must not steer a download elsewhere, and a url
  elsewhere, such as a git host's archive, can hold another package than the
  registry's of that name and version.
- The package client may reach a private address and trusts `ca_cert_file`,
  unlike web research, and fetches https only, with at most 5 redirects and
  100 MiB per archive. Why: the url is the project's own, like a repository
  url, never a model's.
- Of a package only the CSS the project imports, and what that imports, is
  kept, reflowed with a line break after every brace and semicolon, under a
  directory such as `_npm+@acme+ui@1.2.3` at the blueprint's tool root, at
  most 8 MiB per gather, and each kept package is a source of kind `package`.
  Why: the designer needs only the styles, a grep match on a minified
  stylesheet would return the whole file, and `_` starts no project name.
- A package is checked against its npm integrity when its lock entry records
  one, and a mismatch is a gap; a package-lock or classic yarn.lock entry
  without one, and a Yarn Berry package, is kept unverified. Why: Berry's
  checksum covers its own repackaged archive, not the registry's.
- A registry host that fails with a network error is skipped for 5 minutes,
  and a gather's downloads stop after 3 minutes, each a gap. Why: an
  unreachable registry costs one wait every 5 minutes, not one per package.
- A frontend's token values come from the CSS its HTML links and script
  imports load, in load order, following `@import` anywhere in the
  repository but never through a symlink; a relative or aliased link or
  import the tree does not hold is a gap, and a remote or templated link is
  skipped without one. The root's own CSS files none of them reaches are read
  beneath the loaded CSS and count only for tokens it does not declare, with
  a gap when they add one; when none reaches the root's own CSS, every CSS
  file is read in path order, with a gap when there is more than one. Why: an
  unimported theme or a vendored copy must not override the values the app
  shows, and CSS that config or a file the scan skips loads still counts.
- A frontend styled with SCSS, Less, Stylus, Vue or Svelte style blocks or
  CSS-in-JS is a root even with no CSS file of its own, with its frame and
  brand files, and gets a gap naming the style language whether or not it
  also has CSS. Why: those styles are not read, so its tokens and fonts leave
  them out, and a frontend with no root would reach the designer as an empty
  brief.
- Design material that cannot be had is a gap in the brief, and a gather that
  fails is posted as an error the run goes on after. Why: the checkouts alone
  still make a mockup.
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
  `ca_cert_file`, which the LLM, ContextMatrix, git and package clients add to
  it.
  Why: research reaches public hosts only.
- The leak guard refuses paths, file names, code punctuation, declared
  identifiers, module, project and repository names, card ids, and more than
  160 characters, in queries and URL parts; plain words and technology names
  pass. Why: code and project names never leave the installation.
- Knowledge text is not an input of the leak guard. Why: it is the product
  owner's internal material, like the need text, which the guard does not
  block either.

## Security

- The knowledge reads' `caller` is hidden from the model and filled by the
  architect as `blueprint <id> stage <step>`, followed by ` reviewer` or
  ` responder` for those runs. Why: ContextMatrix's knowledge log names who
  asked, and a model cannot name another caller.
- Card and playbook writes and run starts are controller calls made with the
  caller identity, never model tools. Why: the identity that passes
  ContextMatrix's human gates must never reach the model.
- ContextMatrix reads strip `agent_id` and never attach images. Why:
  ContextMatrix then treats the caller as an agent and keeps redacting
  unvetted imported text.
- Credentials embedded in URLs are scrubbed from every result and error of a
  stage's ContextMatrix read tools, and repository URLs are checked out and
  recorded without their userinfo. Why: ContextMatrix returns project config as
  stored, and the record is append-only.
- Every tool result passes the redactor for every secret the session holds
  (the LLM key, MCP key, git token and search key), and live drafts are
  redacted with a held-back tail. Why: a secret never reaches a
  model-visible or stored text.

## Runtime and build

- No timeout on the ContextMatrix MCP client is shorter than 60 seconds. Why:
  ContextMatrix gives a knowledge context or query call up to 55 seconds,
  connect included.
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
