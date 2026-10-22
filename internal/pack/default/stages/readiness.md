# Readiness

Input: every slice's cards, in slicing order, whole, with the need, the mockup when the person approved one, the slices, the shared decisions, and each slice's design, grounding and acceptance, the design and acceptance as digests and grounding as its project facts and verdicts when there are two or more slices (`read_artifact` returns the whole); a read-only checkout of each target project.

For every card, answer with the checkout open: could an agent with only this
repository and this card finish it on the first attempt? Judge one
deliverable, no open decision, no optional scope, a planner split of two
subtasks or fewer. Judge its scope too. A card, or a part of one, that
builds what no decision, contract or criterion of its slice asks for, such
as a guard, a recovery path, a cache or a test hook, is not ready. So is a
card whose only work is documentation, tests or a guard over other cards'
work, a proof of what earlier cards deliver, or a run of the closing
commands; the instruction card that setup asks for is the one exception. The
finding names what to drop, or the card the work belongs in. A need for a
thing asks for one that works: what it needs to build, start, be reached and
do its job end to end for the people who use it, with the data it exists to
keep still there when they come back, is the work itself. The scope rule
cuts what goes beyond that working whole, never the whole. Doing the
asked-for work correctly and securely is the work itself, not an addition: a
security hole, or a wrong result on input the need's users will give, is a
defect. The architect checked every `where` path against the checkout before
you judge: one without `new` exists, and one marked `new` does not exist
yet. Whether a path exists, or whether a file a card creates should be
listed or marked new, is never a finding, in any round: the same `where`
gets the same judgement every time.

A finding is only what would make an agent with only this repository and
this card fail or build the wrong thing, a change that breaks what the code
does today, or scope the blueprint does not ask for. A point the agent can
settle on its own, such as whether a file it touches is new, is no finding.

A card of a project the task names as POC, a proof of concept, is judged on
clarity and buildability, not hardening: Tests expected that covers the main
behaviour and the card's criteria is ready, and one that asks for edge-case
tests its criteria do not demand is a finding. A criterion the card carries
from acceptance is never a finding for being an edge case: the card must
carry it.

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

With an approved mockup (the `mockup` input; read_artifact returns its
source), the architect itself attaches the whole mockup, when it emits the
cards, to every card of a slice that builds a screen. A card's `mockup`
field names only the screens that card builds or changes, and a card that
builds none names none, so never judge whether a card carries the mockup.
Two mockup findings exist, and no other: a card that builds or changes a
screen of the mockup without naming it in `mockup`; and a card that builds or
changes a screen on which the mockup shows something the decisions dropped,
when its Solution does not say not to build that part.

Then judge the blueprint as a whole, each artifact against the others:
decisions, contracts, cards and assumptions that disagree; two assumptions
that say one thing in different words; an outcome (O<n>) that no criterion
or manual check proves; a contract, inside one project or between projects,
that one side's cards keep and the other's do not; a change one card makes
that breaks behaviour the code has today, which no card handles; a decision,
outcome or criterion that traces to no need item, answered question,
accepted assumption or gate feedback and prevents no defect in code that
exists; an assumption that adds work the need does not ask for, accepted or
not. A card that does not deliver what its own criterion promises is that
card's problem, in its `cards` entry; `cross` holds the problems between
artifacts. Report each in `cross`,
`{"id": "X1", "severity": "critical", "major" or "minor", "text": "<one sentence a person can act on>", "refs": ["<the ids it names, such as DEC2, AC4, D3, A5, O1, SL2>"]}`:
critical when building as planned would deliver the wrong thing, break what
the need promises or break what the code does today, or when a decision,
criterion, assumption or card still contradicts the sign-off feedback in the
task, major when someone would have to guess or a part traces to nothing the
blueprint asks for, minor otherwise. `refs` names the cards whose change
fixes the finding, beside the other ids it names; a finding that only a
change to a design or an assumption fixes names no card. A critical or major
finding sends back the cards it names: judge each of them not ready, with
the finding among its own. A minor finding sends nothing back. When nothing
disagrees across the blueprint, `cross` is an empty list or left out; never
invent a finding.

Produce markdown with one `## <card id>: <title>` section per card holding
`ready` or the findings that block it. Submit with a data object
`{"cards": [{"id": "D1", "ready": true, "findings": []}], "cross": [{"id": "X1", "severity": "major", "text": "<the finding>", "refs": ["AC4"]}]}`.
A card with a finding is not ready; judge every card once.
