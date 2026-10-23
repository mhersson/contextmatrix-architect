# A blueprint, step by step

This guide follows one real blueprint, one short chapter per step: a family
asks the architect for a small recipe web app. Every picture comes from that
run. To look something up, such as every reply you can type, see the
[reference](reference.md).

1. [What the architect is](#1-what-the-architect-is)
2. [Before you start](#2-before-you-start)
3. [Starting a blueprint](#3-starting-a-blueprint)
4. [Saying what you need](#4-saying-what-you-need)
5. [The interview](#5-the-interview)
6. [Trying the screen](#6-trying-the-screen)
7. [Cutting the work into slices](#7-cutting-the-work-into-slices)
8. [What every slice shares](#8-what-every-slice-shares)
9. [Designing each slice](#9-designing-each-slice)

## 1. What the architect is

The architect turns something you want built into a plan, and the plan into
cards an AI agent can build.

**ContextMatrix** is a task board. Each piece of work on it is a **card**, and
an AI agent builds what a card describes in your project's code. A card the
agent can finish on its first try takes care to write: what to build, where in
the code, and how to check that it works. The architect does that work with
you. You say what you need in your own words; it interviews you, works the plan
out step by step and writes the cards, and you approve each big decision before
the next step builds on it.

On the way it plays the roles a team would: a product owner who works out the
need, a designer, a system designer, a senior developer and a tester. Each
step's work is checked, by an AI reviewer or by the architect's own rules, and
every decision is recorded with what it rests on. At the end the cards go
on the board as a **playbook**, the cards in the order they are built, or as a
single card when the need is that small.

One need worked out this way, from your first message to cards on the board, is
a **blueprint**. Each blueprint is one chat in ContextMatrix.

![The Cards sheet of the example blueprint: the first slice's two cards, D1 and D2, and the four checks a person does by hand, listed in the order they run](images/01-cards-sheet.png)

This sheet is from near the end of the example: the cards for the first part of
the recipe app, written and checked, before they go on the board.

## 2. Before you start

This step makes sure the architect is there when you open ContextMatrix.

You need ContextMatrix with the architect installed and connected to it, and an
AI model as capable as Anthropic's Claude Sonnet. If **Blueprint** is not
offered when you start a new chat (chapter 3), the architect is not set up yet.
[Installing the architect](installing.md) says what you need and how to get it
running.

## 3. Starting a blueprint

This step creates the blueprint and sets how it runs.

In ContextMatrix's sidebar, click **+ new** beside **Chat**, then choose
**Blueprint**. The dialog's title changes to **New blueprint**:

![The top of the New blueprint dialog: the Chat and Blueprint tiles with Blueprint chosen, and the Title field holding Family recipe app](images/03a-new-blueprint-top.png)

Below the title comes **Projects**, a list of your ContextMatrix projects, each
with a tick box and the name of its code repository. The family ticked
recipe-box. The rest of the dialog:

![The rest of the New blueprint dialog: the four modes with Default chosen, Plain language off, the Model field, the cost cap presets with $10 picked, and the Create blueprint button](images/03b-new-blueprint-settings.png)

| Field | What it sets | In the example |
|---|---|---|
| Title | A name for the blueprint. Left blank, it is named from your first message. | Family recipe app |
| Projects | The ContextMatrix projects the work goes into. Tick at least one. | recipe-box, a new project whose repository holds only a short description file |
| Mode | How much the architect does without you. In **Default** you answer its questions and decide the **gates** that need you, the stops where a step's result is approved. In **Auto**, **Auto + emit** and **Yolo** an AI answers for you. See [Modes](reference.md#modes). | Default |
| Plain language | Asks you only what you want and how it should look and work; the architect makes the technical decisions and tells you what they mean for you. Set here, and kept. See [Plain language](reference.md#plain-language). | Off |
| Model | The AI model the architect uses: one as capable as Claude Sonnet. See [Models](reference.md#models). | A Claude Sonnet model |
| Cost cap | The most the architect may spend on this blueprint, in US dollars: $1, $2, $5, $10 or No cap. See [The cost cap](reference.md#the-cost-cap). | $10 |

Click **Create blueprint**, or press Ctrl+Enter. You can change the cap at any
time, and switch the mode until you approve the **sign-off**, the last approval
before the cards go on the board; both are on the scale bar (chapter 4).

recipe-box is marked as a proof of concept (**POC**) in ContextMatrix, so its
cards ask for tests of the main behaviour and less hardening; the design work is
the same. See [POC projects](reference.md#poc-projects).

**Research**, which lets the architect search the web before it asks you, is a
setting of ContextMatrix, not of this dialog. It was off for this run. See
[The research setting](reference.md#the-research-setting).

## 4. Saying what you need

This step gives the architect the **need**: what you want, in your own words.

The new blueprint opens with one line: "Blueprint created for recipe-box in
default mode. State the need to begin." Type the need in the message box at the
foot of the chat and send it. The family wrote:

> Our family wants a small recipe web app. First, we need to save recipes with
> ingredients and steps, and find them again by name or ingredient. Then we want
> to pick recipes for the week and get one shopping list with everything we
> need.

Write it as you would to a person: who it is for and what they want to do. You
need not know how it should be built; the interview asks for the rest.

![The blueprint pane just after the need was sent: the scale bar across the top, the chat with the need on the left, and the Need sheet on the right while the product owner works](images/04a-blueprint-pane.png)

The blueprint's pane has three parts: the chat on the left, the sheet on the
right and the scale bar across the top.

![The chat after the need: the family's message, Blueprint created for recipe-box in default mode, the Intake stage with the product owner, Checked out recipe-box, and the note that research is off](images/04b-need-in-chat.png)

**The chat** is where you and the architect talk. Under the need, **Intake**,
the first **stage** (one step of the architect's work), has started: the product
owner has fetched a copy of the project's code ("Checked out recipe-box") and is
working on the need. The note that research is off means it works from the
model's own knowledge and your code.

**The sheet** shows each stage's result as it is written, laid out like a page
of a technical drawing. Tabs across the top name each sheet, and a dot marks the
current stage's tab. The title block at the foot says which version it is, who
drew it, who checked it and where it stands.

![The right end of the scale bar: Intake, Product owner is working on the need, then the mode Default and the cost $0.02 of $10.00 above a thin meter](images/04c-scale-bar-cost.png)

**The scale bar** shows the stages from left to right. The striped segment is
the stage in progress, working or waiting for you, and a small triangle marks a
stage with a gate. Beside the bar a line says what the stage is doing or waiting
for, and at the right end are the mode and the cost so far against the cap
($0.02 of $10.00): click either to change it.

Every mark on the sheet and the scale bar is explained in
[The sheet's marks](reference.md#the-sheets-marks). Most of your replies are
buttons; what each one sends, and the commands you can type, are in
[Replies you can type](reference.md#replies-you-can-type). If a stage fails, or
the cost cap runs out, the chat says what happened and how to go on: see
[When a blueprint stops](reference.md#when-a-blueprint-stops).

## 5. The interview

This step is where the architect asks what it cannot decide on its own.

The product owner came back with a **round**: three questions and eight
**assumptions**, things it decided itself, laid out as tabs. A round waits as
long as you need, and your answers are kept if you leave.

![A question round with the tabs Who uses it, Shopping list merge, Week plan shape and Assumptions; the first question shows three options, the first marked Recommended, a box to type your own answer, and Chat about these questions below](images/05-round-question.png)

Each question has its own tab. Click an option, or press its number. The one the
architect suggests is marked **Recommended**, and **pros · cons** shows the case
for and against each. If no option fits, write your own under **Type your own
answer**. To ask about the questions first, write in **Chat about these
questions**: the architect answers there and may change the options. While a
round is open, the usual message box is hidden.

For who uses the app the family picked **Shared, no login**, and for the
shopping list they chose to add up matching items. For the week plan no option
fitted, so they typed their own: "Just a list of the recipes we picked for this
week. We do not plan by day."

![The Assumptions tab of the same round: each assumption with an Accept button and a Turn into a question button, and Accept all at the top](images/06-round-assumptions.png)

The **Assumptions** tab lets you check what the architect decided itself.
**Accept** one you agree with, or **Turn into a question** to have it asked in
the next round; **Accept all** accepts every one. The family accepted all but
one. Assumption 7 said the shopping list is only shown on screen, with nothing
to tick off; they shop with the list on a phone, so they turned it into a
question.

The last tab, **Submit**, lists your answers. **Submit all** sends them, and
**Send ... now, answer the rest later** stores those you have so far. See
[In a question round](reference.md#in-a-question-round).

![The answered round folded into a summary line, then a second round of two questions: whether the shopping list lets you tick items off, with No, view only marked Recommended and Yes, tick off below it](images/07-round-followup.png)

The answered round folds into a summary ("answered by you · 7 accepted, 1
asked"), and the turned assumption came back as a second round: should the list
let you tick items off (Q4), and should it be printed or shared (Q5). The family
chose **Yes, tick off** over the recommended view-only option, and kept the list
on screen only. Questions are numbered across the whole blueprint, so later
steps can point back at each answer.

> [!NOTE]
> **In an auto mode** (Auto, Auto + emit or Yolo) you answer no rounds. An
> **AI responder**, a separate AI run, answers each question in your place and
> accepts the assumptions; the round shows in the chat, read-only. Its answers
> are listed as assumptions at the sign-off. Each gate is decided by the stage's
> reviewer, an AI run that checks the result (more below), instead of you. Auto
> stops at the sign-off; Auto + emit also puts the cards on the board when
> nothing needs a person; Yolo then starts the agent on them too. A gate whose
> result still fails its third review waits for you in every mode. See
> [Modes](reference.md#modes).

### The need, written down

With every answer in, the product owner wrote the **Need** sheet: the problem,
its value, what is in and out of scope, and outcomes that can be measured. Most
lines end with where they came from: [Q1] is the answer to question 1, and
[round1.A3] the third assumption accepted in round 1.

![The Need sheet with an Approved automatically stamp: Problem, Value, Scope in and the start of Scope out, most lines ending in tags such as Q1 or round1.A3; the title block reads Checked by System designer, 4 of 5, Status Approved](images/08-need-sheet.png)

A **reviewer**, a separate AI run that reads the result as the next role would
(here the system designer), scored it 4 of 5 and listed four **findings**,
problems it saw, all minor. A version passes at 4 or more with no critical
finding open. In Default mode Intake's gate is automatic: the reviewer's pass
approves it, so the sheet is stamped **Approved automatically** and nothing
waits for you. See [Gate policies](reference.md#gate-policies) for which gates
wait for you.

![The gate plan, listing who answers each stage's questions and decides its gate, then the Setup note about recipe-box, the System designer reviewed Intake rev 1 passed card, and the line Intake passed its gate under the auto policy](images/09a-gate-plan-and-setup.png)

Two notes came with it. The **gate plan** lists each stage with who answers its
questions and who decides its gate: `human` is you, `auto` the AI. Reply
`gate <stage> human` or `gate <stage> auto` to change one for this blueprint;
see [Gate policies](reference.md#gate-policies). The family kept it as it was.

The **Setup** note lists what the project lacks for the agent to work in it. A
new project like recipe-box has no **verify command** yet, the command (such as
running the tests) that checks the agent's work, and no instruction file for the
agent. Neither blocks anything: the first card writes the instruction file, and
the architect proposes a verify command, which the sign-off asks you to set in
ContextMatrix before the cards run.

## 6. Trying the screen

This step lets you try the screen before the rest is designed.

The recipe app has a screen people will use, so the architect offered a
**mockup**, a working sketch of the screen you can click through:

![The mockup offer in the chat: Your need has a screen people will use. Want a first look at it, a mockup you can adjust, before the architect designs the rest? with the buttons Show mockup and Skip mockup](images/09b-mockup-offer.png)

The family clicked **Show mockup**; **Skip mockup** goes on without one. See
[The mockup](reference.md#the-mockup).

> [!NOTE]
> **A need with no screen**, such as a command that only prints its results, a
> service or a job that runs by itself, has no mockup step: nothing is offered,
> nothing is asked, and the blueprint goes on to slicing.

![Revision 1 on the Mockup sheet: the designer's notes, then a working Recipe box screen with the tabs Recipes, This week and Shopping list, a search box and sample recipes with Add to week and Remove from week buttons](images/10-mockup-rev1.png)

The **designer**, the AI role that draws mockups, showed revision 1 on the
Mockup sheet: a recipe list with search, a This week tab and a Shopping list
tab, all working, with sample recipes to try. Its notes say what to try. For a
project that already has screens, the mockup shows them with the change in
place, in their own look, unless the need asks for a new look.

![The designer's three suggestions under the mockup: Ticked lines switched to Move to bottom, Show recipes behind each line and Show why a recipe matched both off, and the buttons Use this design and Skip mockups](images/11-mockup-suggestions.png)

Under it are the designer's **suggestions**, choices you flip to see the mockup
change at once. Flipping costs nothing. The family moved ticked lines to the
bottom of the shopping list and left the other two off.

![The chat after a change request: the family's message asking for the three tabs at the bottom of the screen on a phone, then Mockup rev 2 and the designer's note on what changed](images/12a-change-request.png)

For a change no switch offers, ask in the chat. The family wrote: "On my phone
the tabs at the top are hard to reach with my thumb. Can the three tabs sit at
the bottom of the screen on a phone?" The designer answered with revision 2.
Each new revision is one AI run, paid from the cap ($0.11 here).

![Revision 2 on the Mockup sheet: the changed note inside a dashed cloud with a small triangle holding 2, and the tabs in a bar at the foot of the phone-sized screen; the title block shows Rev 2 and a Compare box](images/12b-mockup-rev2.png)

Revision 2 puts the tabs in a bar at the foot of a phone-sized screen. The part
that changed sits inside a dashed **revision cloud**, with the revision number
in the small triangle beside it. Revision 2 opened with ticked lines back at
**Stay in place**, so the family moved them to the bottom again. Earlier
revisions stay: the **Rev** list in the title block opens them, and **Compare**
shows what changed (chapter 9).

When it looks right, click **Use this design**. Your settings are recorded as
your answers, and the chat confirms: "You chose revision 2 of the mockup; the
architect designs the rest from it." The cards that build these screens will
carry the mockup with your settings. **Skip mockups** goes on without one.

In an [auto mode](reference.md#modes) there is no offer: the AI responder tries
the suggestions and asks for up to five changes while you watch.

## 7. Cutting the work into slices

This step cuts a big need into **slices**, pieces that each work on their own,
built one after another.

![The Slices sheet: a table of two slices, SL1 Runnable app with recipes and search and SL2 This week and the combined, tickable shopping list, both in the project recipe-box](images/14a-slices-sheet.png)

The product owner put the recipes first, as the base the rest builds on. **SL1**,
"Runnable app with recipes and search", is the app itself, with recipes to save
and find. **SL2**, "This week and the combined, tickable shopping list",
completes the need. After SL1 alone the family already has a searchable recipe
box.

![The scale bar after slicing: the steps that repeat for each slice grouped under SL1 and SL2, and the label Slicing, Slices rev 1 is at your gate](images/14b-scale-bar-slices.png)

The scale bar now groups the stages that run once per slice under SL1 and SL2:
each slice gets its own design, its own check against the code, its own
definition of done and its own cards.

![The review card System designer reviewed Slicing rev 1 passed, no findings, round 1 of 3, and the gate Gate: Slicing, version 1, scored 5 of 5, with the buttons Approve and Reject](images/14c-slicing-gate.png)

This is the first gate that waits for you. The reviewer, the system designer
again, scored the slicing 5 of 5 with no findings. **Approve** accepts the
version on the sheet, and the work goes on. **Reject** opens a box for what to
change: the slicing is done again with your feedback, and the new version is
reviewed again. The same buttons sit in a bar at the foot of the sheet. The
family approved. See [At a gate](reference.md#at-a-gate).

In an [auto mode](reference.md#modes) the reviewer decides this gate for you,
and a version that passes goes straight on.

> [!NOTE]
> **A need with one slice** skips the next chapter's shared decisions, and its
> Slicing gate does not stop on its own: it folds into the slice's design gate,
> where one **Approve** approves both and **Reject slicing** sends the slicing
> back. That holds while both gates have the same policy, as they do unless you
> change one with a `gate` reply. See [Gate policies](reference.md#gate-policies).

## 8. What every slice shares

This step settles the decisions every slice builds on, such as the programming
language and where the data is kept.

![A round of three questions and six assumptions from the system designer, with the tabs Stack, Storage, UI approach and Assumptions; Q9 asks which language and runtime the whole app should be built on, with Python 3, standard library only marked Recommended](images/15-shared-round.png)

With two slices, the system designer, the AI role that designs how the parts
fit together, asked what both share: the **stack** (the language and tools the
app is built with), the storage, and how the page works. These questions are
technical. If you cannot judge them, the recommended option and its pros and
cons are the architect's advice. With [plain language](reference.md#plain-language)
you would not be asked them: the architect decides, and shows you what each
decision means for you, to accept or turn down.

The family took every recommendation (Python with nothing extra to install, the
data in a single database file, and one web page served by the app) and
accepted all six assumptions.

![The Shared sheet: decisions DEC1 to DEC4, each with what was chosen, what was rejected and why, and its basis such as Q9 or round4.A2; at the foot a gate bar with Approve, Reject and Findings (2), and the title block Checked by Senior developer, 4 of 5, At your gate](images/16-shared-sheet.png)

The **Shared** sheet lists each decision as DEC1, DEC2 and so on: what was
**Chosen**, what was **Rejected** and why, and its **Basis**, the answer it rests
on (Q9 is question 9, round4.A2 the second assumption accepted in round 4). The
numbers jump past the interview's Q5 and round 2 because the mockup counted as
round 3, and its three settings were recorded as questions 6 to 8.

![The review card Senior developer reviewed Shared decisions rev 1 passed, opened to its summary and two findings, R1 and R2, both minor and new, above the gate with Approve and Reject](images/17-shared-review.png)

This time the reviewer was the senior developer, the AI role that later writes
the cards. It scored the version 4 of 5 with two minor findings. Click the review
card to read its summary and findings, each with an id (R1, R2), how serious it
is (minor, major or critical) and whether it is new this round; **Findings (2)**
on the sheet's gate bar lists them too. Minor findings do not send a version
back: it comes to the gate with them, and you decide. The family approved.

> [!NOTE]
> **With one slice** this step is skipped: the slice's own design settles these
> decisions too.

## 9. Designing each slice

This step designs one slice in detail: how its parts work and fit together.

![A round of nine assumptions from the system designer for SL1, with only the Assumptions tab, Accept all, and each assumption with Accept and Turn into a question](images/18-design-assumptions.png)

Solution design runs once per slice, SL1 first. For SL1 the system designer
asked no questions, only listed nine assumptions to check, so the round had just
the Assumptions tab. The family accepted all nine.

### When a reviewer sends a version back

![The chat for SL1's design: Design SL1 rev 1, the review card sent back with 4 new findings, Design SL1 rev 2 Revised for the 4 findings, the review card passed with 4 fixed and 1 new in round 2 of 3, and the gate with Approve, Reject, Reject slicing and Reject shared decisions](images/19a-design-reviews.png)

The senior developer reviewed version 1 and **sent it back** with four new
findings, one of them major: the page had to drop blank ingredient rows before
saving. A version goes back when it scores below 4 or leaves a critical or major
finding open, and you need do nothing: the system designer wrote version 2,
"Revised for the 4 findings", and the reviewer checked it. Version 2 **passed**:
4 fixed, 1 new, in round 2 of 3. Hover over the **?** beside a verdict for the
rule.

Each version is reviewed once, and at most three versions are reviewed before
the gate. If the third review still does not pass, the gate waits for you,
whatever the mode.

![The Solution design sheet for SL1, version 2: slice tabs SL1 and SL2 under the sheet tabs, decisions DEC1 and DEC2 inside dashed revision clouds, and a gate bar with Approve, Reject, Reject slicing, Reject shared decisions and Findings (5)](images/19b-design-sheet.png)

On the sheet, a row of slice tabs picks the slice, and the decisions that
changed in version 2 sit in revision clouds. **Findings (5)** on the gate bar
lists all five findings.

![The round-2 review card opened: the reviewer's summary, one minor finding R5 new this round, and Fixed (4) folded below](images/20-design-review-round2.png)

In the chat, open the review card to read the reviewer's summary, the finding
that is new this round, and, folded under **Fixed (4)**, the four findings
version 2 fixed.

![The Solution design sheet with Compare ticked: removed words struck through on red and added words on green in DEC1 and DEC2](images/21-design-compare.png)

Tick **Compare** in the title block to read version 2 against version 1: removed
text is struck through on red, and added text sits on green.

With two or more slices, a slice's design gate has two more buttons.
**Reject slicing** sends the slicing back with your feedback, and everything
after it runs again. **Reject shared decisions** sends the shared decisions
back, and every slice is designed again. **Reject** runs this slice's design
again with your feedback. See [At a gate](reference.md#at-a-gate). The family
approved version 2.

### The next slice

Before SL2's design starts, SL1 goes through the rest of its stages: the
architect checks its design against the code, writes what done means and writes
its cards. In Default mode the AI responder answers their questions, and
Acceptance's reviewer decides its gate; the other two have no gate (see
[Gate policies](reference.md#gate-policies)). So in the example they ran without
stopping. Then SL2 gets its own design:

![The scale bar during SL2's design: every SL1 step filled, the SL2 group with its label cut off at SL2 · This week and the co, and the label Solution design, SL2, Design rev 1 is at your gate](images/22-scale-bar-sl2.png)

SL2's design listed twelve assumptions and no questions, and the family accepted
them. The reviewer passed version 1 at 4 of 5 with two minor findings, and the
family approved it. By then the blueprint had spent $2.39 of its $10 cap.
