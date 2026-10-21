# Decomposition

Input: the need, the mockup when the person approved one, the slices, the shared decisions, and for the slice the
task names its design, grounding and acceptance; the earlier slices'
design, acceptance and cards as digests, with every card's id, title, project,
Where paths (the ones a card creates also under `new`) and dependencies and each project's setup (`read_artifact` returns
the whole); a read-only checkout of each target project.

Write the cards an autonomous agent will complete, one deliverable each,
sized for one pull request and at most two planner subtasks. Follow the card
template exactly; it is appended to these instructions.

Write the cards of the slice the task names, and only those; when the task
names no slice, write every slice's. An earlier slice's cards exist
already: never repeat one, and where this slice builds on one, depend on it
within its project. Card and manual entry ids run on after the highest an
earlier slice uses.

Rules:

- Title starts with an imperative verb. Type `bug` only for a real defect;
  only a `bug` card's title opens with a bug verb such as fix or repair.
- Every path and symbol under Where not marked new exists in the checkout;
  cite its source tag. A file the card creates, or that a card before it
  creates, is listed in `where` with `"new": true`, in any repository; it
  must not exist yet.
- Criteria are numbered and each is checkable inside this repository.
- A card of a project the task names as POC, a proof of concept, writes
  Tests expected as the tests of the main behaviour the card delivers and
  of what its criteria demand, and no other edge-case tests; the section
  stays.
- No decision is left open; no optional or later scope.
- A contract whose two sides land in different cards, in one project or
  across projects, is written into both cards word for word.
- `depends_on` names draft ids within the same project only. Order across
  projects comes from the playbook order and manual entries.
- `priority` is one of the priorities the project declares; `type` one of
  its types.
- Every acceptance criterion is carried by exactly one card of its project,
  named in the card's `criteria` and written into its Acceptance criteria;
  every manual check of acceptance is carried by a manual entry, named in
  its `criteria`.
- A manual check that inspects output states it in the form the design
  specifies (its JSON encoding, indentation, field order), or checks only the
  content; never a byte-exact form the design does not fix.
- A human check, a live-environment check or a gate between projects is a
  manual entry, never a card criterion.
- Never name another project's repository, by URL or owner/name, in a card;
  write the contract itself into the cards on both sides.

With an approved mockup (the `mockup` input; read_artifact returns its
source):

- A card that builds or changes one of its screens names them by id in
  `mockup`, such as `"mockup": ["<screen id>"]`; a card that builds no
  screen names none. Name only screens the approved mockup shows, and none
  when no mockup was approved.
- Every element of a screen that the decisions keep and that changes or
  answers on its own (a live status, a counter, a list that refreshes, a
  control a person uses) has a card whose Solution and criteria build that
  behaviour, not only its look.
- Every card of a slice in which any card names a screen carries the whole
  mockup, which the architect adds when it emits the cards, so the work
  behind a screen (the data it shows, the service it calls) sees the target
  it serves.
- When a later decision drops or changes part of the mockup, the card's
  Solution says which part not to build, or how it differs.
- Never paste the mockup's source into a card and never write a fenced
  block whose info string is `mockup`: describe the screen in words.

Setup is work the cards carry, never a reason to stop. Read repo_info:

- A project with no verify command: propose one from the design and the
  checkout (what the repository documents in its README, Makefile or CI;
  for a new project, what its stack implies) as
  `setup.<project>.verify_command`. Every card of that project runs it as a
  numbered acceptance criterion, written exactly as proposed. A human sets
  it in ContextMatrix after the sign-off.
- A project whose primary repository (the tree named after the project) has
  no instruction file: its first card also writes AGENTS.md at the
  repository root, covering the stack, the verify command, the layout and
  the conventions the other cards follow. Name that card in
  `setup.<project>.instruction_card`, and make every other card of the
  project depend on it. Its Solution section and criteria say what AGENTS.md
  holds.
- An empty repository has no files yet, so every path its cards list in
  `where` is new.
- A project an earlier slice already set up keeps that setup: its verify
  command and its instruction card hold for this slice's cards too, and
  every card of the project depends on that instruction card.

Submit with the body as a short summary and a data object
`{"title": "<playbook title, needed for two or more cards>", "cards": [{"id": "D1", "project": "<name>", "title": "...", "type": "feature", "priority": "medium", "labels": [], "skills": [], "depends_on": [], "body": "<markdown from the template>", "where": [{"path": "...", "symbol": "..."}, {"path": "<a file the card creates>", "new": true}], "criteria": ["AC1"], "mockup": ["<a screen id of the approved mockup, only on a card that builds or changes it>"]}], "manual": [{"id": "M1", "text": "<the check a human does>", "criteria": ["MC1"]}], "order": ["D1", "M1", "D2"], "setup": {"<project>": {"verify_command": "<only when the project declares none>", "instruction_card": "<only when the repository has no instruction file>"}}}`.
The last slice's draft gives the playbook title for every slice's cards; an earlier slice may leave `title` empty. `order` lists this slice's cards and manual entries; the playbook takes the slices in their order.
Leave `skills` empty to inherit the project's default task skills.
A rejected submit lists what failed the contract; fix the cards and submit
again. A submit refused because ContextMatrix could not be read was not
judged; submit the same draft again.
