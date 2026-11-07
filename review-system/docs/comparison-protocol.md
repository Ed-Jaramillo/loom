# M1 comparison protocol (delivery row W1.1.1)

Status: protocol and schema only. No review has been run under this protocol yet.
Running it requires `G-RUN`, which is not recorded.

## Question

Does the thin seven-stage review pipeline find defects the v7.1 single-review
process misses, and at what operator and model cost, on the same frozen input?

## Arms

| Arm | Meaning |
|---|---|
| baseline | The v7.1 review prompt in one review session. Generated prompt, recorded hash |
| pilot | The seven logical stages through one verified Loom runtime. Tracks run in sequence |

Both arms use the same frozen bundle (`bundle.Export`), the same models, and the
same budget caps from `pilot.Budget`. A budget or model mismatch blocks the
comparison. It cannot support a speed or cost claim.

## Input

One `pilot.Profile` per comparison, validated by `Profile.Validate`:

- `identity`: owner, repo and PR number. The PR under review.
- `source`: head and base refs. The bundle pins them to SHAs at export time.
- `runtime`: the execution path. If none is verified, record `runtime.blocker`.
  A blocked profile is valid, but `Runnable()` returns false.
- `models`: requested provider, model and settings. Requested values are not
  observed provider behavior.
- `budget`: wall time, tool calls, model calls and cost. Each must be positive.
  An absent value is a rejected profile, not "no cap".

## Session register

`pilot/session-register.json` lists delivery rows. Each row names its repo
(R or L), branch, worktree, app session and PR URL. PR URLs stay empty until
PRs are opened, as the plan records for this stage. A blocked row needs a
blocker. A done row needs evidence. The register is validated on load and save.

## Pass and fail rules

- A changed head, base or diff base during export fails the export.
- An incomplete changed-file listing fails the export.
- A changed profile after a comparison started creates a new comparison ID.
- Unknown cost and unknown operator minutes stay unknown in every report.

## Not yet defined here

Findings adjudication, pairing of findings across arms, and the workload
report belong to W1.1.5 and W1.3.x. They are not part of this protocol revision.
