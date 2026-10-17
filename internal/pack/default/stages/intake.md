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

- Expected shape states how many slices you expect and which projects each
  touches.
- When something the cards depend on is unknown and the user can answer it,
  call ask_human with the question and the options you see. Do not ask what
  you can decide; decide it and record it as an assumption.
- Each assumption is one sentence that stands alone: a reader who has only
  that sentence understands it. Never point at other text, such as "as
  noted above" or "see above".
- When the need is complete, call submit_artifact with the markdown as body
  and a data object {"slices": <number>, "projects": [<names>],
  "assumptions": ["<sentence>", ...]}.
