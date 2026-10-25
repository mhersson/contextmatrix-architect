# Unattended

This blueprint is unattended: nobody watches the playbook's run, so it must
not stop for a person except at a real gate. The playbook runner stops at
every manual entry in `order` until a person checks it off. A manual entry
in `followups` is a check a person makes after the run, and never stops it.
Only decomposition and readiness work differently; every other stage works
as always.

## Decomposition

For this blueprint these rules replace three parts of your instructions:
that order across projects comes from manual entries, that a gate between
projects is a manual entry, and the example order `["D1", "M1", "D2"]`.

- Order between projects comes from `order` alone: a card comes after the
  cards it builds on, in any project. A manual entry never only orders
  projects, since `order` does that already; a real gate between projects
  stays in `order` like any other real gate.
- Every manual entry goes in `followups`, never in `order`, wherever its
  check would otherwise sit, unless it is a real gate. That holds for
  setting a secret ("Add `WEATHER_API_KEY` to the environment"), since a card
  that reads it from the environment is built and tested with a stand-in,
  and for every check that only confirms the result: a person looking at
  the output, a check in a live environment, a manual check of acceptance.
  A human check is still a manual entry, never a card criterion.
- A real gate is a step only a person can take that a later card cannot
  work without, such as signing up for an outside service whose sandbox a
  later card's criteria call. It stays in `order`, before the first card
  that needs it, as a last resort. Its `note` says in one sentence why the
  run must wait there, worded to follow "because", such as "the next card
  calls the payment sandbox, which only a person can sign up for".
- So the example order for this blueprint is cards only, such as
  `["D1", "D2"]`, with the checks in `followups`, such as `["M1"]`. Add
  `"followups": ["<manual id>"]` to the data next to `order`; each manual id
  is in exactly one of the two.
- When readiness's findings carry `needless_stops`, each names a manual
  entry still in `order` and why the run need not stop there. Move it to
  `followups`, or keep it in `order` with a `note` that answers the finding.

## Readiness

Judge every manual entry still in `order` against the rule above: is it a
step only a person can take that a later card cannot work without? One that
is not is a needless stop: a check that only confirms the result, setting a
secret a card reads from the environment, or a gate between projects that
`order` already gives. Report each in the data's `needless_stops`,
`[{"id": "M1", "text": "<why the run need not stop there>"}]`, naming only
manual entries in `order`, each once. A needless stop never goes in a card's
findings or in `cross`, and never makes a card not ready. A manual entry in
`followups` is never one. With none, `needless_stops` is an empty list or
left out.

Judge every manual entry in `followups` too: the run never waits for it, so
a card that cannot work without it would fail. That card is not ready, and
its finding names the entry.
