# Plain language

This blueprint runs in plain language: the person driving it is not
assumed to be technical. Only what they are shown changes. Your technical
questions, decisions, assumptions and artifacts are recorded and used exactly
as always.

## Stages

Tag every question's `audience`:

- `experience`: what the person wants to do with the result, who uses it,
  how it should look, feel and behave, and what matters most.
- `technical`: how it is built. When in doubt, it is technical.

Never ask a technical question in everyday words. "Should data live on the
device or a server?" is technical. "Should it work without internet?" is
experience. Word experience questions, their options, pros and cons in
everyday words, with examples from the person's own world: a shared shopping
list, a salon's booking page, a tool that renames holiday photos, a club's
member list, a recipe planner.

For each assumption whose effect a person using the result would notice, give
`consequence`: one plain sentence on what it means for them, true to the
decision. Give none when they would notice nothing. Example: for "bookings
are stored per salon, not per stylist", the consequence is "Each stylist
cannot keep a separate calendar."

A consequence uses no technical word: no protocol, format or security term
(such as HTTPS, token or JSON), no field, file, setting or command name, and
no id the architect gives (such as DEC5, A4, Q2, D3 or X1). Say what the
person will notice or have to do; the assumption's own text keeps the
technical detail.

The sign-off shows the person the slices' titles and values, the need's
outcomes and the consequences they accepted: word those in everyday language
too, keeping each outcome measurable. A finding across the blueprint
(`cross`) is shown to them as well: its `text` is one plain sentence on what
is wrong for them and what to do, with no id or technical word; `refs` keeps
the ids, and the markdown body the technical detail.

When the person turns a consequence down, rethink the decision behind it. Ask
technical follow-ups as technical questions, and re-ask a responder answer
with `reasks`.

## Responder

With each answer whose effect a person using the result would notice, add
`"consequence": "<one plain sentence on what it means for them>"` to the data.
Leave it out otherwise. Example: for a club's member list answered with
"members sign in with an emailed link", the consequence is "Members need
access to their email to get in."

## Reviewer

Judge each experience question listed: is it a technical question in
disguise? Judge each consequence against its technical text: does it state the
effect truly, in plain words? Raise a finding for each that fails. A
consequence or outcome that uses a technical word or an id is a finding.
