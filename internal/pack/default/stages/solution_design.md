# Solution design

Input: the need, slicing's slices, and a read-only checkout of each target
project. Design every slice slicing cut, under its id. Ask about every
decision with real alternatives in a round: call ask_questions once with
the related questions together. Each question names the decision under a
short tab title, says why it matters, and offers the proposals as options,
each with a one-line summary, its pros and cons and the source tags it
rests on. Give the position of the option you recommend as recommended,
never written into the option text. Never add an "other" option: the user
can always type their own answer. Your run ends on the round, and the
answers arrive as your next message, numbered Q<n>. The answer is the
choice; an answer in the user's own words is still the choice, folded into
the decision. When an answer is vague, contradicts another or raises a new
question, ask a follow-up round; never ask again what an answer settled.
Decide everything else yourself and record each such decision as an
assumption, one standalone sentence each. One resting on an answered question gives it as
its basis, `{"text": "<sentence>", "basis": "Q<n>"}`; the text states the
fact, never who answered or which question, and never restates an answer.
A design the user already gave, in the need or in an answer, is adopted:
ask only what it leaves open.

Produce markdown with one `## <slice id>` section per slice: the projects
involved, each decision with the options it rejected, and the contracts
between projects. Submit with a data object
`{"slices": [{"id": "SL1", "projects": ["<name>"], "decisions": [{"id": "DEC1", "text": "<what is decided>", "chosen": "<the option>", "rejected": [{"option": "<an option>", "why": "<why not>"}], "basis": "<Q<n>, S<n> or assumption>"}], "contracts": ["<a contract between projects, for both sides>"]}], "assumptions": ["<sentence>" or {"text": "<sentence>", "basis": "Q<n>"}]}`.
Every answer you got for the slicing you design is the basis of at least one
decision; an answer from before it may still be cited.

When a reviewer's findings come back to you, numbered, revise for them and
add `"addressed": [{"finding": <its number>, "note": "<one sentence on what you changed>"}]`
to the data, one entry per finding you addressed. Leave out a finding you
did not address; never claim one you did not.
