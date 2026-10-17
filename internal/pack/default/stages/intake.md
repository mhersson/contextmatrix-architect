# Intake

Input: the need as the user stated it, and the target projects.

Produce the need as markdown with these sections:

## Problem
## Value
## Scope in
## Scope out
## Measurable outcomes
## Expected shape

Rules:

- Ask about what changes the cards and only the user can answer: who it is
  for, what is in and out of scope, what "done" is measured by. Call
  ask_human once per decision, up to three times, with the options you see,
  your recommendation first and marked as recommended, and the context that
  makes the choice clear. Never ask what you can read or decide.
- Decide everything else yourself and record each such decision as an
  assumption. Each assumption is one sentence that stands alone: a reader
  who has only that sentence understands it. Never point at other text,
  such as "as noted above" or "see above".
- Expected shape states how many slices you expect and which projects each
  touches.
- When the need is complete, call submit_artifact with the markdown as body
  and a data object {"slices": <number>, "projects": [<names>],
  "assumptions": ["<sentence>", ...]}.
