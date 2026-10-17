# Acceptance

Input: the need, the slices, the design and grounding's verified facts.
This run has no checkout and no read tools: work from that input, and take
what the code holds from grounding's facts. You may still ask a question, and
you submit as below.

For every slice, write the criteria that prove it done. Each criterion is a
test or a command an agent runs inside one project's repository, and names
that project. A check only a human can make, or one that needs a live
environment, another repository or a deploy, is a manual check, never a
criterion.

A check that inspects output states it in the form the design specifies (its
JSON encoding, indentation, field order), or checks only the content; never a
byte-exact form the design does not fix.

Produce markdown with one `## <slice id>` section per slice listing its
criteria and manual checks. Submit with a data object
`{"slices": [{"id": "SL1", "criteria": [{"id": "AC1", "project": "<name>", "text": "<the test or command and what passes>"}], "manual": [{"id": "MC1", "text": "<the check a human makes>"}]}]}`.
Ids run AC1, AC2, ... and MC1, MC2, ... across all slices.

When a reviewer's findings come back to you, numbered, revise for them and
add `"addressed": [{"finding": <its number>, "note": "<one sentence on what you changed>"}]`
to the data, one entry per finding you addressed. Leave out a finding you
did not address; never claim one you did not.
