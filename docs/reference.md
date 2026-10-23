# Architect reference

This page is for looking things up while you run a blueprint: what you can
type, the modes and settings, the cost cap, what to do when a blueprint stops,
and what the marks on the sheet mean. Developers will find how the architect
works inside in [design.md](design.md).

## Replies you can type

You run a blueprint from its chat in ContextMatrix. Most replies are buttons,
and a click sends the reply for you. What a gate's buttons send can also be
typed, and the commands below can only be typed; answers to a question round
and choices on the mockup sheet have buttons of their own. Capital letters
never matter.

A blueprint is worked out in **stages**, steps of AI work such as Slicing or
Solution design, run one after another (the **pipeline**). A big need is cut
into **slices**, pieces that each work on their own (SL1, SL2 and so on), and
the design stages then run once per slice.

### Commands

A command is the whole message: `export` on its own is a command, "export the
list as CSV" is ordinary text. Commands are taken at any point, also while a
stage is working, and are never read as an answer; the table says when each
one has an effect.

| What you type | What it does | When it works |
|---|---|---|
| `abort` | Stops the run that is working now. The blueprint is kept, and an open round stays open. | Any time. Afterwards `continue` runs the stage again. |
| `continue` or `resume` | Runs the stopped stage again, once. | After a stage failed, was aborted or was stopped by the cost cap. See [When a blueprint stops](#when-a-blueprint-stops). |
| `export` | Writes the blueprint's whole record as a set of text files on the architect's server, and says where. | Any time. |
| `cap <amount>`, such as `cap 5` or `cap 7.50` | Sets the cost cap, in US dollars. | Any time. The amount must be above what is spent. See [The cost cap](#the-cost-cap). |
| `cap off` | Removes the cost cap. | Any time. |
| `mode default`, `mode auto`, `mode auto+emit`, `mode yolo` | Switches the mode. | Until the sign-off is approved, and not after the pipeline has ended. See [Modes](#modes). |
| `gate <stage> human`, `gate <stage> auto` | Says who answers a stage's questions and decides its gate, for this blueprint. | Any time. It applies from that stage's next round or gate. See [Gate policies](#gate-policies). |
| `retrospective` | Looks at the cards on the board that struggled (many review rounds, stuck, or far more costly than the rest) and proposes **lessons**, rules that improve the cards of later blueprints; you decide each one in a lesson round. | Once the cards are on the board, while no run is working and no lesson round is open. |
| `lessons` | Lists every lesson from every blueprint, with its state, its scope, its reason and the cards behind it. | Any time. |
| `retire L<n>`, such as `retire L3` | Retires an active lesson: blueprints created afterwards no longer get it. | Any time, for an active lesson only. |

Two commands are strict about their shape. `cap` takes a plain number without
a dollar sign (`cap 5`, not `cap $5`). `gate` takes exactly three words, with
the stage written as one word: `gate shared_decisions human`.

### At a gate

A **gate** is a stop after a stage: the stage's result waits on the **sheet**,
the drawing beside the chat, until it is approved or rejected. A gate's replies
are buttons in the chat and at the foot of the sheet.

| What you type | What it does | When it works |
|---|---|---|
| `approve` | Accepts the version on the sheet; the pipeline goes on. | At an open stage gate, with nothing after it. |
| `reject <feedback>` | Runs the stage again with your feedback; the new version is reviewed again. | At an open stage gate. A bare `reject` only asks what to change. |
| `reject slicing <feedback>` | Runs Slicing, and everything after it, again. | At a Solution design gate (button "Reject slicing"). |
| `reject shared_decisions <feedback>` | Runs Shared decisions, and every slice, again. | At a slice's Solution design gate, when the blueprint has two or more slices (button "Reject shared decisions"). |

Any other text at a gate is kept as a note, and the gate stays open with a
reminder of its replies. Put a space after a stage's name, not a colon: at a
stage's gate, `reject slicing` with nothing after it is a plain `reject` whose
feedback is the word "slicing".

### At the sign-off

The **sign-off** is the last gate. It lists the cards in order, the assumptions
the plan rests on, and the result of a last check against ContextMatrix (the
**preflight**: does every card's project exist and have a repository, and is
there anything to warn about). In Default and Auto mode it waits for you.

| What you type | What it does | When it works |
|---|---|---|
| `approve` | Puts the cards on the board in ContextMatrix (**emits** them), with a **playbook**, the cards' run order, when there are two or more. | At the sign-off, with nothing after it. Over preflight warnings the button reads "Approve and override warnings"; the reply is the same. |
| `approve <reason>` | Approves, and records your reason. | When Readiness, the stage that checks every card, left cards not ready or found a critical problem across the plan (buttons "Approve over open findings" and "Approve over critical findings"). A bare `approve` then asks for the reason. |
| `reject <feedback>` | Runs the blueprint again from its first decisions, with your feedback: from Shared decisions when they ran, else from the first slice's Solution design. | At the sign-off. |
| `reject slicing <feedback>` | Runs Slicing, and everything after it, again. | At the sign-off. Typed only. |
| `reject shared_decisions <feedback>` | Runs Shared decisions, and every slice, again. | At the sign-off, when Shared decisions ran. Typed only. |
| `reject SL<n> <feedback>`, such as `reject SL2 ...` | Runs that slice's Solution design, and everything after it, again; earlier slices stay. | At the sign-off, with two or more slices. Typed only. |

The typed rejections have no buttons; the sign-off's text lists the ones that
apply. A rejection that names no step of this blueprint is kept as a note, with
a hint.

### After the sign-off

While the session is open, the architect **watches** the emitted cards on the
board and posts each change in the chat.

| What you type | What it does | When it works |
|---|---|---|
| `approve` | Picks up where emission stopped, starts a run that Yolo held back, or tries a failed run start again. Nothing is created twice. | After an approved sign-off, when the chat offers "Resume emission", "Start the run" or "Retry the run start". |

### The mockup

When the need has a screen people will use, the architect draws a live
**mockup**, a working sketch of the screen, before it designs the rest. In
Default mode it asks first.

| What you type | What it does | When it works |
|---|---|---|
| `show mockup` | The designer, the AI role that draws mockups, draws a first one. | While the offer waits (button "Show mockup", recommended). |
| `skip mockup` | Goes on without a mockup. | While the offer waits, and after `show mockup` until a first mockup is shown, for example when drawing it failed. |
| Any text | A change request ("Can you add tabs for each shop?"): the designer answers, or draws the next revision. | While a mockup waits for you on the sheet. |

On the mockup sheet you click rather than type: flipping a suggestion changes
the mockup at once and costs nothing, "Use this design" records your settings,
and "Skip mockups" goes on without one. Any other text while the offer waits is
kept as a note, and the offer stays open.

### In a question round

A **round** is the questions a stage asks together, one tab each in the chat,
with the stage's **assumptions** (what it decided itself) on the last tab. A
round waits as long as you need, across days, a session's end and restarts.

| What you do | What it does | When it works |
|---|---|---|
| Pick an option, or "Type your own answer" | Answers that question. The recommended option is marked. | On each question's tab. |
| "Accept", "Accept all" or "Turn into a question" | Decides each assumption. One you turn into a question is asked in the next round. | On the Assumptions tab. |
| The "Submit all" button | Sends every answer, and the stage goes on. | On the Submit tab, once everything is answered. |
| The "Send ... now, answer the rest later" button | Stores the answers so far; the round keeps waiting for the rest. | On the Submit tab. |
| Type a message | Chat about the round: the stage answers in the round's chat and may change or add questions. A question whose options change loses its answer. | While a round waits for you. |

After a retrospective, a **lesson round** asks about each proposed lesson, with
the options "Approve for *project*" (the project of the card that struggled),
"Approve for the whole pack" (every later blueprint, whatever its project) and
"Reject". Your own words reword the lesson and approve it at the scope it
proposed. A message typed while a lesson round waits is kept as a note.

### Anything else

| What you type | What it does | When it works |
|---|---|---|
| Your first message | The **need**: what you want, in your own words. Intake, the first stage, starts on it. | When there is no need yet, and again after Intake failed before it stored anything. |
| Any other text | A **note**: kept, and given to every AI run that starts later. If a stage stopped, it also runs that stage again. A lone `approve` or `reject` is kept but given to no run. | When nothing above takes it. While a stage works, the note reaches only the runs after it. After the pipeline has ended, nothing runs. |

## Modes

The mode decides how much the architect does without you. You pick it in the
New blueprint dialog, and you can switch it later.

| Mode | Questions and stage gates | Sign-off | Cards on the board | Run started |
|---|---|---|---|---|
| Default | You answer and decide where a stage's policy says `human`; the AI does the rest (see [Gate policies](#gate-policies)). | Waits for you. | When you approve. | No: you start it in ContextMatrix. |
| Auto | The AI answers every question and decides every gate. | Waits for you: your only stop. | When you approve. | No. |
| Auto + emit | As Auto. | Approves itself when nothing needs a person, else waits for you. | Yes. | No. |
| Yolo | As Auto. | As Auto + emit. | Yes. | Yes, unless a card's repository has no commits yet. |

Where the AI answers, an **AI responder**, a separate AI run that does not see
how the asking stage reasoned, answers each question in your place; its answers
are recorded as assumptions and listed at the sign-off. Where the AI decides a
gate, the stage's **reviewer** does (see [Gate policies](#gate-policies)).

"Nothing needs a person" means the preflight finds nothing blocking and gives
no warning, Readiness passed every card, and no critical problem was found
across the plan. The sign-off is then stamped approved automatically.

The mockup follows the mode too. In Default mode the architect offers it first
and you steer it. In the other modes there is no offer: the AI responder tries
the suggestions and asks for up to five changes while you watch.

Yolo starts the cards in ContextMatrix. A single card ends as one pull request
to its repository's default branch; several run as a playbook whose pull
requests merge into a playbook branch, and the final merge into your default
branch stays yours. If a card's repository has no commits yet, the cards go on
the board and the run waits until you push a first commit and reply `approve`.
How the agent then builds each card is described in
[ContextMatrix's own docs](https://github.com/mhersson/contextmatrix). The cost
cap limits the architect only; ContextMatrix's own per-card limits bound the
agent.

### Switching mode

Click the mode next to the cost on the scale bar and pick another ("Switch and
run" for Yolo), or type `mode <mode>`. The new mode applies from the next
question, gate or sign-off, and the architect posts its gate plan again.
Switching is possible until the sign-off is approved or the pipeline ends; the
mode then reads "Mode locked". A switch to the mode already in effect changes
nothing.

| Switch | What happens to what waits for you |
|---|---|
| Into Auto, Auto + emit or Yolo | A round waiting for you goes to the AI responder: the answers you sent stay yours, and it answers the rest. Answers picked but not sent are dropped. A waiting mockup, or mockup offer, goes to the responder. A stage gate shown only because its policy asked you is approved if its review passed. |
| Into Auto + emit or Yolo, at the sign-off | The sign-off is decided again, and approves itself unless something needs a person. |
| Back into Default | A round the responder took stays with it. A mockup comes back to you. No gate is decided. |

A `gate <stage> human` reply outranks the mode: that stage stays with you. A
gate shown because its review did not pass stays with you in every mode.

## Gate policies

Every stage has two policies: who answers its questions, and who decides its
gate. Each is `human` (you) or `auto`.

- Under `auto`, the AI responder answers the questions, and the stage's
  reviewer decides the gate. The **reviewer** is an AI run that reads the
  result as the next role would (the system designer reads the need and the
  slices, the senior developer the designs, the tester the acceptance
  criteria), scores it from 1 to 5 and lists its findings. A version is
  approved when it scores 4 or more with no critical finding open. Before
  that, a version that scores lower or leaves a critical or major finding open
  is revised and reviewed again, up to three reviews; if the third review
  still does not pass it, the gate waits for you whatever its policy says.
- Under `human`, the gate shows you the version with its reviewer's score and
  findings, and you `approve` or `reject` it.

The policies in Default mode:

| Stage | What it does | Its questions | Its gate |
|---|---|---|---|
| Intake | Works out the need | You | Auto: its reviewer decides |
| Mockup | Draws the screen, when there is one | You: the offer, and steering the mockup | No gate |
| Slicing | Cuts the work into slices | You | You. With one slice it is decided together with Solution design's gate |
| Shared decisions | Settles what every slice shares | You | You. Skipped with one slice |
| Solution design | Designs each slice | You | You |
| Grounding | Checks each design against the code | Auto | No gate |
| Acceptance | Writes what done means for each slice | Auto | Auto |
| Decomposition | Writes each slice's cards | Auto | No gate |
| Readiness | Checks every card, and sends one that is not ready back to Decomposition, up to three rounds | Auto | No gate |
| Sign-off | Your final approval | - | You in Default and Auto, itself in Auto + emit and Yolo |

In Auto, Auto + emit and Yolo every policy is `auto`. With plain language
every stage gate is `auto`, and questions keep the policies above. A `gate`
reply outranks both.

With one slice, Slicing's gate **folds** into Solution design's when both have
the same gate policy: one gate judges both, `approve` approves both, and
`reject slicing <feedback>` runs Slicing again.

### Changing a policy

Type `gate <stage> human` or `gate <stage> auto`, with the stage written as one
of `intake`, `mockup`, `slicing`, `shared_decisions`, `solution_design`,
`grounding`, `acceptance`, `decomposition` or `readiness`.

- It sets both policies of that stage, for this blueprint and for every slice.
- It applies from the stage's next round or gate: a round, gate or mockup offer
  already open stays with whoever it was put to.
- For `mockup` it decides who is offered the mockup and who steers it, from its
  next revision.
- `gate signoff ...` changes nothing: the sign-off follows the mode. A stage
  name the blueprint does not have changes nothing either, and the reply lists
  the stages.

Once Intake has finished, the architect posts a **gate plan**: every stage's
policies and this reply, so you can change a policy before the bigger stages
run. It is posted again after a mode switch.

## The cost cap

The cost cap is the most the architect may spend on one blueprint, in US
dollars. It covers every AI run the architect makes for the blueprint: the
stages, their reviewers, the AI responder, the mockup designer and the
retrospective. It does not cover the agent building the cards; ContextMatrix's
own per-card limits do.

| Where | What you see and do |
|---|---|
| New blueprint dialog | The "Cost cap" field: pick an amount, or No cap. A capped blueprint needs a model ContextMatrix has a price for, and the dialog says so. |
| The scale bar | The cost at its right end, such as "$1.20 of $5.00", with a thin meter that changes colour at 80% and turns red at the cap. Without a cap it reads "$1.20, no cap". |
| Click the cost | A small box with what is spent and the cap: +$2, +$5 and +$10 buttons, a "Cap in USD" field, a "No cap" box, and Save. |
| Type it | `cap <amount>` or `cap off`: the same change. |

How it behaves:

- A new cap must be above what is already spent, or nothing changes ("Nothing
  changed: the blueprint has spent $2.14; a cap must be above that, or reply
  `cap off`.").
- A change applies at once, also to a run that is working, from its next call
  to the model: a raise lets it go on, and a lower cap it has already passed
  stops it.
- A run starts only if the cap leaves at least what such a run usually costs
  on this blueprint, and never less than $0.10.
- If the cap leaves too little for the AI responder, the stage answers that
  question itself and goes on.
- Each run also has the architect's own per-run limit, set by whoever runs the
  architect. A run that reaches it fails like any failed run.
- Reopening the session keeps the cap the blueprint stored.

### When the cap stops a stage

A cap stop names the stage and says how to go on, for example: "The
blueprint's $2.00 cap is used up ($2.14 spent); Readiness stopped. Raise the
cap by clicking the cost in the header (or reply `cap <usd>`) and it
continues." The blueprint waits and keeps everything it has.

- Raise the cap above the one it stopped under, or remove it: the stopped
  stage runs again at once ("Cost cap raised from $2.00 to $4.00; resuming
  Readiness.").
- If the cap was already raised while the run was ending, the message says
  "The cap is now $4.00: reply `continue` and it goes on."
- `continue` without a raise runs the stage again, and it meets the cap again.
- A retrospective the cap stopped does not resume on a raise: raise the cap,
  then reply `retrospective`.

## When a blueprint stops

| What happened | What you see | How to go on |
|---|---|---|
| A stage failed | A message naming the stage and why, ending "Reply `continue` to run it again." Its step on the scale bar reads Failed. | `continue`, or send a message: it is kept as a note, and the stage runs again with it. |
| You sent `abort` | "Abort requested. ... is canceled; reply `continue` to run it again." | `continue`, or any message. |
| Intake failed before it stored anything | As for any failed stage. | Your next message is taken as the need again; `continue` runs Intake on the need you already sent. |
| The cost cap stopped a stage | See [When the cap stops a stage](#when-the-cap-stops-a-stage). | Raise the cap. |
| Emission stopped, or ContextMatrix blocked it | "Emission stopped: ... Cards already created are kept; reply approve to resume.", or "Emission is blocked until this is fixed in ContextMatrix; then reply approve". | Fix what it names, then `approve` ("Resume emission"). |
| Yolo found a repository with no commits | "... has no commits, so the run was not started; push an initial commit, then reply approve to start it, or start it in ContextMatrix and reply approve." | Push a first commit, then `approve` ("Start the run"). |
| The run did not start | "The cards are emitted, but the run did not start: ..." | `approve` ("Retry the run start"), or start the run in ContextMatrix and reply `approve`. |
| The pipeline ended | The scale bar reads "Stopped:" and a reason, such as "the cards still failed their contract checks after 3 submits". | Nothing more runs in this blueprint; start a new one. |
| A retrospective stopped | "The retrospective stopped: ... reply `retrospective` to run it again." | `retrospective`. |

### When the architect restarts

If the architect's server restarts, ContextMatrix starts the session again by
itself, at the latest when you next send a message; you need not do anything.
Nothing finished is lost:

- A stage that was working runs again from its inputs, or from its stored
  conversation after a round. The text it was writing on the sheet is written
  again.
- A round comes back with the answers you had sent. A gate or the sign-off is
  shown again as it was.
- An emission cut short carries on, creating nothing twice, and the watch on an
  emitted blueprint's cards starts again.
- A retrospective cut short is not run again: the chat says so once, and you
  reply `retrospective`.

### Ending and reopening a session

"End session", in the chat pane's menu (the three dots), disconnects the chat
from the architect. A run that is working is cancelled and the watch on the board
stops, but the blueprint is kept. "Reopen", in the same menu, starts it again:

- A stage the end cut short runs again, with the answers it had been given.
- A round comes back with its stored answers and waits, however long you were
  away. A gate or the sign-off is shown again.
- The watch on an emitted blueprint's cards starts again.
- The mode, the cost cap and plain language stay as the blueprint stored them.

A lesson round is not shown again on a reopen: scroll back to it in the chat
and answer it there.

### `continue` and `resume`

The two words do the same: they run the stopped stage again, once, without
adding a note. They work after a failed or aborted stage, review or revision,
after a reply in a round's chat failed, and after a cap stop, where the stage
meets the cap again unless you raised it. The chat answers "Resuming" and the
stage's name.

When nothing is stopped they start nothing, and the chat says "Nothing is
waiting to resume.": a round, gate, mockup offer or sign-off waiting for you
needs your answer instead, a working stage needs no help, and an ended
pipeline runs no more. An emission or a run start resumes with `approve`, and
a retrospective with `retrospective`.

## Plain language

Plain language is for someone who can say what they want but not how it is
built. Switch on "Plain language" in the New blueprint dialog; it is set when
the blueprint is created and never changes after. The chat pane's header then
shows "Plain language".

| What | With plain language |
|---|---|
| Questions | You are asked only about the experience: what you want, who uses it, how it should look and behave. The AI responder answers the technical questions on a read-only round of their own. |
| Technical decisions | Shown on the assumptions tab as consequences, what they mean for you. Accept one, or turn it down: your concern goes back and the architect asks again. Accepting a consequence does not confirm the technical decision; the record says a person accepted its consequence. |
| Stage gates | Decided by the reviewers, as in Auto, unless you reply `gate <stage> human`. A gate the review did not settle comes to you. |
| Where the architect is stuck | Gates, the sign-off, a stopped emission or run start, and an ended pipeline are worded plainly, with the technical text folded underneath for a technical helper. The buttons read "Continue anyway" (or "Continue" when the review passed) and "Try again", and send `approve` and `reject` as usual. A stopped emission or run start offers "Try again", and a run waiting for a first commit "Done, continue"; both send `approve`. |
| The sign-off | Opens with a plain summary: what will be built, what you will be able to do with it, and the consequences you accepted, each marked if it changed since. |

What stays the same: the cards, the sheets and the export are technical, and a
technical helper can read every folded detail. The stages, the reviewers and
the cost cap work as always, and the sign-off follows the mode. Plain language
works with every mode; in the Auto modes you read the plain wording but answer
nothing.

## The research setting

Research lets the interviewing stages (Intake, Slicing, Shared decisions and
Solution design) search the web and read public pages before they ask, so the
options in a question can cite the pages behind them. It is a setting of
ContextMatrix, made by whoever runs it, and it is off by default; it is not in
the New blueprint dialog. It is read each time the session starts, so a change
reaches a blueprint the next time it is opened.

| Research | What happens |
|---|---|
| Off | The stages work from the model's own knowledge and your code. The chat says once that research is off, and question options show "model knowledge" instead of web links. |
| On | Those stages search the web (with Brave Search) and read pages, and options link the pages read. Reviewers may open a cited page to check it, but do not search. A guard keeps your project's own names, code, file paths and card ids out of every search and page address. Search result lists are never stored; only the pages read are kept, as sources. |

## POC projects

A project marked "Proof of concept project" in ContextMatrix's Project Settings
(the sidebar shows it with a POC badge) gets cards that ask for less hardening.
The design work does not change.

| Part of the blueprint | For a POC project |
|---|---|
| Intake, Slicing, Shared decisions, Solution design and Grounding | Unchanged: a POC still gets a sound design. |
| Acceptance | Skips the edge-case sweep (the check of unusual input and failure cases) for a slice whose projects are all POC. In a slice that also touches a production project, an edge case that only the POC project has is recorded as not applicable, "POC project". The AI responder answering its questions knows which projects are POC. |
| The tester (Acceptance's reviewer) and Readiness | Judge a POC's criteria and cards on clarity and buildability, not hardening. |
| Decomposition | A card's "Tests expected" asks for tests of the main behaviour and of what its criteria demand, and no other edge-case tests. |
| Everything else | Unchanged: the stages, gates, rounds, reviewers and their pass marks, costs and caps, and every other card rule. |

The flag is read from ContextMatrix as each Acceptance, Decomposition and
Readiness run starts, so switching it during a blueprint reaches only the runs
that start afterwards; cards keep the bar they were designed with. If
ContextMatrix cannot be read then, that run fails, and your next message or
`continue` runs it again. How the agent builds a POC card is described in
[ContextMatrix's own docs](https://github.com/mhersson/contextmatrix).

## Models

The architect needs a Sonnet-class model: Anthropic's Claude Sonnet, or one as
capable. In testing, smaller models sent empty or broken answers when a stage
handed in a large result, and the architect does not repair a broken answer,
so the stage fails.
See [Installing](installing.md).

Pick the model in the New blueprint dialog's "Model" field. Every role in the
blueprint uses it, unless whoever runs the architect has set a model for a
particular role. A blueprint with a cost cap needs a model ContextMatrix has a
price for.

## The sheet's marks

A blueprint's pane shows the **scale bar** across the top and the **sheet**,
where every result is drawn like a page of a technical drawing, beside the
chat or over it on a narrow pane. Tabs along the top of the sheet name
each sheet: Need, Mockup, Slices, Shared, Design, Grounding, Acceptance, Cards,
Readiness, Sign-off and Board. A dot marks the current stage's tab.

### The scale bar

One segment per step of the pipeline, left to right.

| Segment | Meaning |
|---|---|
| Filled | Done. |
| Filled and narrow | Folded: decided at a later gate. |
| Striped and moving | Working now: writing, being reviewed or revising. |
| Striped and still | Waiting for you. |
| Red outline | Failed, or stopped the pipeline. |
| Dashed outline | Not reached: the pipeline ended before it. |
| Empty outline | Not started yet, or skipped. |
| Small triangle above it | The step has a gate. |

Hover over a segment for its name, its slice, the role that works on it, its
status and its cost; click a done one to open its sheet. Beside the bar a line
says what the current step is doing or waiting for, followed by the mode and
the cost, which you click to change.

### The slice row

With two or more slices, the scale bar brackets each slice's steps under its
id (SL1, SL2 and so on), with the title of the slice in progress, and a sheet
that repeats per slice gets a row of slice tabs under the sheet's tabs; the
slice waiting for you is highlighted. With one slice, nothing about slices
shows.

### The title block

At the foot of every sheet, as on a drawing.

| Field | Shows |
|---|---|
| Sheet | Which sheet it is. |
| Rev | The version shown. With several, a list to pick another, each marked superseded, rejected, folded or writing where that applies. |
| Compare | A tick box, when an earlier version exists. See [Compare](#compare). |
| Drawn by | The role that drew it: product owner, designer, system designer, senior developer or reviewer. |
| Checked by | The reviewer and its score, such as "System designer, 4 of 5", with "failed" when it did not pass; click it for the findings. "Contract checks" on a sheet checked by rules alone. |
| Status | Where this version stands, such as Writing, Waiting on your answers, Being reviewed, At your gate, Approved, Rejected, Superseded or Done. |

### Stamps

A decided version carries a stamp on the sheet.

| Stamp | Meaning |
|---|---|
| Approved, by you with the time, or automatically | The version passed its gate. |
| Rejected, by you with the time | You sent it back. |
| Superseded, by rev N | A newer version replaced it. |
| Folded, into design | Its gate was decided together with Solution design's. |

### Revision clouds

On the newest version of a sheet that has earlier ones, each part that changed
since the version before sits inside a dashed cloud, with a small triangle in
the margin holding the version number, as a drawing marks a revision.

### Compare

Tick Compare in the title block to read the version against the one before it:
removed text is struck through on a red tint, and added text sits on a green
tint.
