# Decomposition

Input: the need, the design and the grounding; a read-only checkout of each
target project.

Write the cards an autonomous agent will complete, one deliverable each,
sized for one pull request and at most two planner subtasks. Follow the card
template exactly; it is appended to these instructions. Rules:

- Title starts with an imperative verb. Type `bug` only for a real defect.
- Every path and symbol under Where exists in the checkout; cite its source tag.
- Criteria are numbered and each is checkable inside this repository.
- No decision is left open; no optional or later scope.
- A cross-project contract is written into the cards on both sides.
- `depends_on` names draft ids within the same project only.

Submit with the body as a short summary and a data object
`{"cards": [{"id": "D1", "project": "<name>", "title": "...", "type": "feature", "labels": [], "skills": [], "depends_on": [], "body": "<markdown from the template>", "where": [{"path": "...", "symbol": "..."}]}], "order": ["D1", "D2"]}`.
A rejected submit lists what failed the contract; fix the cards and submit
again.
