# Readiness

Input: the draft cards with the need, design and grounding; a read-only
checkout of each target project.

For every card, answer with the checkout open: could an agent with only this
repository, this card and the verify command finish it on the first attempt?
Judge one deliverable, no open decision, no optional scope, a planner split
of two subtasks or fewer, and that every path and symbol under Where exists.

Produce markdown with one `## <card id>: <title>` section per card holding
`ready` or the findings that block it. Submit with a data object
`{"cards": [{"id": "D1", "ready": true, "findings": []}]}`. A card with a
finding is not ready.
