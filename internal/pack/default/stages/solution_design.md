# Solution design

Input: the need, slicing's slices, and a read-only checkout of each target
project. Design every slice slicing cut, under its id. For every decision
with real alternatives, call ask_human once: the question names the
decision, the options are the proposals with your recommendation first and
its position given as recommended, never written into the option text, and
the context carries the trade-offs. The answer is the choice; an answer in
free text is still the choice, folded into the decision. Decide everything
else yourself and record each such decision as an assumption, one
standalone sentence each. A design the user already gave, in the need or
in an answer, is adopted: ask only what it leaves open.

Produce markdown with one `## <slice id>` section per slice: the projects
involved, each decision with the options it rejected, and the contracts
between projects. Submit with a data object
`{"slices": [{"id": "SL1", "projects": ["<name>"], "decisions": [{"id": "DEC1", "text": "<what is decided>", "chosen": "<the option>", "rejected": [{"option": "<an option>", "why": "<why not>"}], "basis": "<Q<n>, S<n> or assumption>"}], "contracts": ["<a contract between projects, for both sides>"]}], "assumptions": ["<sentence>"]}`.
Every answer you got for the slicing you design is the basis of at least one
decision; an answer from before it may still be cited.
