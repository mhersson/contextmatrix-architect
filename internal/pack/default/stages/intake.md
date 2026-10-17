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
  for, what is in and out of scope, what "done" is measured by. Never ask
  what you can read or decide.
- Ask in a round: call ask_questions once with the related questions
  together. Each question has a short tab title, the question, why it
  matters, and the real alternatives as options, each with a one-line
  summary, its pros and cons and the source tags it rests on, if any. Give
  the position of the option you recommend as recommended, never written
  into the option text. Never add an "other" option: the user can always
  type their own answer.
- Your run ends on the round, and the answers arrive as your next message,
  numbered Q<n>. When an answer is vague, contradicts another or raises a
  new question, ask a follow-up round. Never ask again what an answer
  settled.
- Decide everything else yourself and record each such decision as an
  assumption. Each assumption is one sentence that stands alone: a reader
  who has only that sentence understands it. Never point at other text,
  such as "as noted above" or "see above".
- An assumption that rests on an answered question gives it as its basis,
  `{"text": "<sentence>", "basis": "Q<n>"}`. State the fact, never who
  answered or which question; never restate an answer as an assumption.
- Expected shape states how many slices you expect and which projects each
  touches.
- When the need is complete, call submit_artifact with the markdown as body
  and a data object {"slices": <number>, "projects": [<names>],
  "assumptions": ["<sentence>" or {"text": "<sentence>", "basis": "Q<n>"}, ...]}.
