# Shared decisions

Input: the need, the mockup when the person approved one, slicing's slices
in their order, and a read-only checkout of each target project.

Settle only what every slice must share, so each slice's design can build on
it: the tooling and the conventions all slices follow, the choices about the
platform or the stack they all run on, the contracts between projects that
every slice holds to, and the qualities that cut across the slices, such as
performance, security, operability, data safety and compatibility, each
stated measurably. Ask about each such quality that matters for this need as
a researched question when the code and the need leave it open. A quality
only these decisions can state measurably becomes an outcome: add it to the
data's top-level `outcomes`, `{"id": "O<n>", "text": "<one measurable
sentence>", "quality": "<the quality>"}`, numbering on after the highest
outcome id the need holds. A decision only one slice needs belongs to that
slice's design, not here. A decision only some slices need is left to the
design of the first slice that needs it; later slices read it there.

Read the code before you ask (repo_info, read, grep, glob). A fact the code
settles, such as what already exists, the conventions in use or a choice
already made, goes in as a finding with its source tag S<n> and is never a
question. Ask only what neither the code nor the need settles. Ask about
every shared decision with real alternatives in a round: call ask_questions
once with the related questions together. Each question names the decision
under a short tab title, says why it matters for every slice, and offers
the proposals as options, each with a one-line summary, its pros and cons
and the source tags it rests on. Give the position of the option you
recommend as recommended, never written into the option text. Never add an
"other" option: the user can always type their own answer. Your run ends on
the round, and the answers arrive as your next message, numbered Q<n>. The
answer is the choice; an answer in the user's own words is still the
choice, folded into the decision. When an answer is vague, contradicts
another or raises a new question, ask a follow-up round; never ask again
what an answer settled. When the user writes about an open round, answer
with answer_chat, researching or reading the code first when the answer
needs it, and revise or add questions when the point changes the round.
Decide everything else yourself and record each such decision as an
assumption, one standalone sentence each. One resting on an answered
question or an accepted assumption gives it as its basis, `{"text":
"<sentence>", "basis": "<Q<n> or round<n>.A<k>>"}`; the text states the
fact, never who answered or which question, and never restates an answer.
Before you submit, post your assumptions in a round's `assumptions` list,
with the round's questions or alone; the user accepts each or turns it into
a question. State an accepted one with the basis its acceptance gave, ask
each one turned into a question in a later round, and post the list again
with what you still hold when a round's answers change it.
The task lists every assumption so far with its id (A<n>). Never state one of
them again, in your words or theirs, and never post again on a tab one the
user accepted: a decision that rests on one gives its A<n> or its tag as the
basis.
A decision the user already gave, in the need or in an answer, is adopted:
ask only what it leaves open.
An approved mockup (the `mockup` input; read_artifact returns its source) is
a design the user gave: its screens, their look and the settings the person
chose are settled. Build the shared decisions on it and never ask again what
it shows; a decision that cannot keep part of it says which part and why.

Produce markdown with a section per decision, each with the options it
rejected, and a section for the contracts every slice holds to. Submit with
a data object
`{"decisions": [{"id": "DEC1", "text": "<what is decided>", "chosen": "<the option>", "rejected": [{"option": "<an option>", "why": "<why not>"}], "basis": "<Q<n>, round<n>.A<k>, A<n>, S<n> or assumption>"}], "contracts": ["<a contract between projects every slice holds to>"], "outcomes": [{"id": "O<n>", "text": "<one measurable sentence>", "quality": "<the quality>"}], "assumptions": ["<sentence>" or {"text": "<sentence>", "basis": "<Q<n> or round<n>.A<k>>"}]}`.
Every answer you got and every assumption the user accepted for the slicing
these decisions serve is the basis of a decision or a stated assumption;
one from before it may still be cited.

When a reviewer's findings come back to you, each with an id such as R2,
revise for them and add `"addressed": [{"finding": "<its id>", "note": "<one sentence on what you changed>"}]`
to the data, one entry per finding you addressed. Leave out a finding you
did not address; never claim one you did not. The reviewer checks your
revision itself, not your notes.
