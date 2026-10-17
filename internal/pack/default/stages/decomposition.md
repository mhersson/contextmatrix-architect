# Decomposition

Input: the need, the slices, the design, grounding and acceptance; a read-only
checkout of each target project.

Write the cards an autonomous agent will complete, one deliverable each,
sized for one pull request and at most two planner subtasks. Follow the card
template exactly; it is appended to these instructions. Rules:

- Title starts with an imperative verb. Type `bug` only for a real defect;
  only a `bug` card's title opens with a bug verb such as fix or repair.
- Every path and symbol under Where exists in the checkout; cite its source tag.
- Criteria are numbered and each is checkable inside this repository.
- No decision is left open; no optional or later scope.
- A cross-project contract is written into the cards on both sides.
- `depends_on` names draft ids within the same project only. Order across
  projects comes from the playbook order and manual entries.
- `priority` is one of the priorities the project declares; `type` one of
  its types.
- Every acceptance criterion is carried by exactly one card of its project,
  named in the card's `criteria` and written into its Acceptance criteria;
  every manual check of acceptance is carried by a manual entry, named in
  its `criteria`.
- A human check, a live-environment check or a gate between projects is a
  manual entry, never a card criterion.
- Never name another project's repository, by URL or owner/name, in a card;
  write the contract itself into the cards on both sides.

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
  project depend on it. Its Change section and criteria say what AGENTS.md
  holds.
- An empty repository has no files yet. `where` lists only paths that
  exist, so its cards name the files they create in the Where section and
  leave `where` empty.

Submit with the body as a short summary and a data object
`{"title": "<playbook title, needed for two or more cards>", "cards": [{"id": "D1", "project": "<name>", "title": "...", "type": "feature", "priority": "medium", "labels": [], "skills": [], "depends_on": [], "body": "<markdown from the template>", "where": [{"path": "...", "symbol": "..."}], "criteria": ["AC1"]}], "manual": [{"id": "M1", "text": "<the check a human does>", "criteria": ["MC1"]}], "order": ["D1", "M1", "D2"], "setup": {"<project>": {"verify_command": "<only when the project declares none>", "instruction_card": "<only when the repository has no instruction file>"}}}`.
Leave `skills` empty to inherit the project's default task skills.
A rejected submit lists what failed the contract; fix the cards and submit
again. A submit refused because ContextMatrix could not be read was not
judged; submit the same draft again.
