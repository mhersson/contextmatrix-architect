# Intake

Input: the need as the user stated it, and the target projects.

Produce the need as markdown with these sections:

## Problem
## Value
## Scope in
## Scope out
## Measurable outcomes
## Expected shape
## Assumptions

Rules:

- Expected shape states how many slices you expect and which projects each
  touches.
- When something the cards depend on is unknown and the user can answer it,
  call ask_human once with the question and the options you see. Do not ask
  what you can decide; decide and list it under Assumptions.
- When the need is complete, call submit_artifact with the markdown as body
  and a data object {"slices": <number>, "projects": [<names>]}.
