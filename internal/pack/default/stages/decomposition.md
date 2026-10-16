# Decomposition

Input: the need, the design and the grounding; a read-only checkout of each
target project.

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
- A human check, a live-environment check or a gate between projects is a
  manual entry, never a card criterion.
- Never name another project's repository, by URL or owner/name, in a card;
  write the contract itself into the cards on both sides.

Submit with the body as a short summary and a data object
`{"title": "<playbook title, needed for two or more cards>", "cards": [{"id": "D1", "project": "<name>", "title": "...", "type": "feature", "priority": "medium", "labels": [], "skills": [], "depends_on": [], "body": "<markdown from the template>", "where": [{"path": "...", "symbol": "..."}]}], "manual": [{"id": "M1", "text": "<the check a human does>"}], "order": ["D1", "M1", "D2"]}`.
Leave `skills` empty to inherit the project's default task skills.
A rejected submit lists what failed the contract; fix the cards and submit
again.
