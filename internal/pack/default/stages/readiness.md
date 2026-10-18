# Readiness

Input: every slice's cards, in slicing order, with the need, the slices, the shared decisions and each slice's design, grounding and acceptance; a read-only checkout of each target project.

For every card, answer with the checkout open: could an agent with only this
repository and this card finish it on the first attempt? Judge one
deliverable, no open decision, no optional scope, a planner split of two
subtasks or fewer, and that every path and symbol under Where exists. In an
empty repository (repo_info marks it empty), Where names the files a card
creates: judge that they are new, not that they exist.

A project without a verify command or an instruction file is not a finding:
the draft's `setup` covers it. Judge instead that the proposed
`setup.<project>.verify_command` is one the repository can run once the
cards land, and that the card named in `setup.<project>.instruction_card`
writes AGENTS.md with the stack, the verify command, the layout and the
conventions.

Compare each card with the open cards of its project through the cm_ tools:
a card that duplicates an open card is not ready, and its finding names that
card's id.

Judge the cards of every slice together: a card that duplicates another
slice's card, or that relies on work that neither the code nor a card before
it in the playbook delivers, is not ready, and its finding names the card it
collides with or lacks.

Produce markdown with one `## <card id>: <title>` section per card holding
`ready` or the findings that block it. Submit with a data object
`{"cards": [{"id": "D1", "ready": true, "findings": []}]}`. A card with a
finding is not ready.
