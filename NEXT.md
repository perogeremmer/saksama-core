# What is next

Short by design: it lists what was deliberately left undone, why, and what would make
each one finished. Anything that is not here is either done or not yet a decision.

State when this was written: `v0.5.0` released, `feat/wizard-style` at `bddd910`
(the wizard/colour/plan-card commit), CI green, working tree clean.

## 1. A spinner during `apply`

**Why it was left out.** `apply` already prints its own progress lines
(`[ 42%] copying`, throttler status, row counts). A spinner writing carriage returns on
the same line would fight those for the terminal, and the loser would be whichever wrote
last.

**What finished looks like.** The long stretch of `apply` that prints nothing - waiting
for the copy to start, waiting for a cutover lock, polling the session - gets the same
one-line indicator the wizard uses on connect and plan, and the progress lines keep their
own line untouched. Needs the indicator moved somewhere both frontends can use it
(`internal/render`, not `internal/wizard`), and a test that the two writers never
interleave on one line.

## 2. Colour inside tables

**Why it was left out.** `render.Table` measures column widths from cell text. An escape
sequence counted as characters misaligns the row, and the truncation path can cut one in
half. The advisory report and the engine list therefore print a plain `ok` / `FAIL` /
`warn` while the surrounding summary line is coloured.

**What finished looks like.** The table measures visible width (strip escapes before
measuring, re-apply after padding, never truncate inside a sequence), then the status
cells can be coloured like everything else, and a test asserts every row is still the
same width with colour on and off.

## 3. Clearing the screen between wizard screens

**Not planned.** It would delete the scrollback, and the previous plan card is exactly
what an operator scrolls back to. The screens are separated by a rule instead. Revisit
only if someone asks for it, and then as a flag that defaults to off.

## 4. Colouring `--json` output

**Never.** The JSON is a contract for scripts; colour would be a defect. Anything that
reads the JSON reads bytes, and the contract does not know whether it was written to a
terminal.

## Closed, for the record

- Findings printed twice on the plan screen - fixed by deleting the duplicate renderer
  rather than keeping two lists in sync (`bddd910`).
- `-` as menu filler and the fixed 30-column gutter - replaced by columns measured from
  the items.
- Colour reaching a pipe, a CI log or a captured session - prevented by
  `render.ShouldColor`, and asserted by `TestColourNeverChangesTheText`.
