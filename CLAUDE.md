# Working rules for agents in this repo

Non-negotiable. Read before touching anything.

## 1. Verify before you hand anything over

Nothing gets delivered until the full gate has actually been run and has
actually passed, on the code being delivered:

```bash
make lint        # golangci-lint + gofmt
make test        # go test ./...
make test-web    # vitest
make test-e2e    # playwright
```

"It builds" is not verification. "The tests I wrote pass" is not
verification. The gate is what CI runs, so a green gate locally is the
only claim worth making — and it is the only claim to make.

Two specific failures this rule exists because of:

- **Run the package more than once.** A test that passes once can still be
  racing. `go test ./path/ -count=1` in a loop, or `-shuffle=on`, catches
  what a single green run hides. A flake handed over is a broken build
  handed over.
- **Match the target toolchain.** `rust-toolchain.toml` / `go.mod` pin
  versions for a reason: a newer linter has different lints. Verifying on
  a toolchain the recipient does not have is not verifying.

If something cannot be verified here — no ffmpeg, no macOS, no hardware —
say so explicitly and name what was *not* checked. Do not let an
unverified thing travel as if it were checked.

## 2. Delivery is files in place, not a git puzzle

Write the changed files directly into the working tree. No branches to
fetch, no bundles to unpack, no merges to resolve.

Hand over the exact commands, with **explicit paths**:

```bash
git add path/one.go path/two.tsx
git commit -m "…"
```

Never `git add -A` — other work may be in the tree, and sweeping it into
someone else's commit is not a small mistake.

**If the delivery mechanism changes for any reason, the exact working
commands ship in the same message as the deliverable.** Not the next
message. Not on request.

## 3. Versioning

Every user-visible change bumps `VERSION` and updates the docs it
affects (`docs/settings.md`, `docs/usage.md`). ADRs live in `docs/adr`
and are the authority on behaviour — read the relevant one before
changing what it describes.
