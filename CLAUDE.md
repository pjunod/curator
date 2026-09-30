# Working rules for agents in this repo

Read before touching anything. These rules reflect the owner's September 30,
2026 workflow and replace the earlier in-place delivery and repeated-test rules.

## 1. Work in your own clone

Never work in the owner's repositories under `~/code`. Create an independent
clone outside that directory and use a `codex/` branch. Do not leave source,
build output, test artifacts, or agent scratch files in the owner's checkout.
Clean up your clone, temporary files, processes, and remote branch after merge.
Preserve other people's work; remove only artifacts you created.

## 2. Batch delivery through a reviewed pull request

Keep building until the requested work is complete. Make coherent commits and
batch related changes into one substantial PR rather than opening a PR for
every small edit. Stage explicit paths so unrelated changes stay out.

Only when the PR is ready to merge into `main`, obtain an adversarial agent
review. Address its findings before running the unit tests and required CI
checks. Do not run tests on every incremental edit. After that final gate,
rerun checks only for failures, review fixes, or substantive changes that
invalidate the result. Merge after review issues are resolved and checks pass.
Do not bypass required branch protection.

The final verification commands are:

```bash
make lint        # pinned golangci-lint + gofmt
make test        # Go unit and integration tests
make test-web    # web unit tests
make test-e2e    # browser acceptance tests
```

Use the toolchains pinned by the project and CI. Report anything that cannot
be verified; never describe an unverified check as passing.

## 3. Keep progress and decisions visible

Create and maintain a status page with implementation progress, PR link,
review findings, final validation, merge status, and cleanup. When the owner
is unavailable, choose a reasonable course and record the decision for later
review. Surface conflicting directives to the owner rather than hiding them.

## 4. Ship usable features

Do not gate features in code. If an enable control is absolutely necessary,
put it in an enable section in the Dev settings tab. Explain what is needed
to enable it safely and whether those conditions are met. That information is
advisory; it must not disable the enable control. Do not add unused flags or
other cruft.

Every user-visible change bumps `VERSION` and updates the docs it affects
(`docs/settings.md`, `docs/usage.md`). ADRs live in `docs/adr`; read relevant
ones before changing the behavior they describe.

## 5. Every action must have a visible result

A click, tap, or keyboard action must reveal its panel, form, confirmation,
result, or error inside the current viewport. Never append the only response
above or below the screen and expect the user to find it. Use the shared
`web/src/ActionDialog.tsx` for detached action panels, or explicitly reveal
inline results while accounting for fixed navigation. Keep loading and
completion feedback visible, including actions near the bottom of long lists.
Move keyboard focus into opened work, support dismissal, and return focus to
the trigger. Check desktop and phone viewport geometry in browser tests;
`toBeVisible()` alone does not prove that an element is on screen. Background
polling must not move the user's scroll or focus.
