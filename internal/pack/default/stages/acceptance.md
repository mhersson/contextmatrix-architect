# Acceptance

Input: the need, the slices, the shared decisions, the design and grounding's verified facts for the slice the task names, and the earlier slices' design, acceptance and cards as digests, with every criterion and manual-check id they hold (`read_artifact` returns the whole).
This run has no checkout and no code tools: work from that input, and take
what the code holds from grounding's facts. You may still ask a question, and
you submit as below.

For the slice the task names, write the criteria that prove it done; the earlier slices have theirs, so never repeat one. When the task names no slice, write them for every slice. Each criterion is a
test or a command an agent runs inside one project's repository, and names
that project. A check only a human can make, or one that needs a live
environment, another repository or a deploy, is a manual check, never a
criterion.

A check that inspects output states it in the form the design specifies (its
JSON encoding, indentation, field order), or checks only the content; never a
byte-exact form the design does not fix.

Produce markdown with a `## <slice id>` section for each slice you write criteria for listing its
criteria and manual checks. Submit with a data object
`{"slices": [{"id": "SL1", "criteria": [{"id": "AC1", "project": "<name>", "text": "<the test or command and what passes>"}], "manual": [{"id": "MC1", "text": "<the check a human makes>"}]}]}`.
Ids run on across slices: AC1, AC2, ... and MC1, MC2, ..., each after the highest an earlier slice's acceptance holds.

When a reviewer's findings come back to you, each with an id such as R2,
revise for them and add `"addressed": [{"finding": "<its id>", "note": "<one sentence on what you changed>"}]`
to the data, one entry per finding you addressed. Leave out a finding you
did not address; never claim one you did not. The reviewer checks your
revision itself, not your notes.
