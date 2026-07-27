# Migrations — read this before adding one

Embedded into the binary (`//go:embed migrations/*.sql`) and applied by goose
at startup. `db.Migrate` is fatal on error: a server that cannot migrate does
not run.

## The one rule that has actually bitten us

**goose identifies a migration by the number in its filename and by nothing
else.** No checksum, no name comparison. Two branches that each add an
`0020_*.sql` are, to goose, the *same migration*: whichever build runs first
records version 20, and the other file is skipped **permanently and
silently** — no error at startup, nothing in the log. The symptom arrives
later and somewhere else, as a SQL error from a binary whose code is entirely
correct:

```
SQL logic error: table download_clients has no column named mode (1)
```

Several agents work in this repo at once, on separate branches, against one
shared development database. So:

- **Before writing a migration, check every branch, not just yours:**
  `git log --all --diff-filter=A --name-only -- internal/infra/sqlite/migrations/`
  Take the next number above the highest anyone has claimed. `ls` is not
  enough — it shows your branch, and your branch is not where the collision
  comes from. (Asked how this is known: migration 23 was claimed twice, the
  second time by whoever wrote this file.)
- **Two files with the same number in ONE directory panic goose at
  startup**, by name, immediately. That case is loud and cheap. The silent
  one — the one this document exists for — is when the two never meet on
  disk: separate branches, one shared database, and the loser skipped
  forever with no error anywhere.
- **If your branch and another land on the same number, renumber before
  merging** — not afterwards. Once a database has recorded the number, the
  loser is unrecoverable by ordinary means and needs a repair migration.
- **A migration that has shipped is immutable.** Editing one changes nothing
  on any database that already ran it. Append a new migration instead.

Two guards exist because the above already happened once:

- `migration0021.go` — a Go migration that inspects the schema and repairs
  whatever `0020_nzbd_native.sql` was supposed to do, whether or not goose
  thinks it ran. Go rather than SQL because SQLite has no
  `ADD COLUMN IF NOT EXISTS`.
- `DB.verifySchema` — checked after every migrate. A column the query layer
  writes to but the database lacks is fatal at startup, naming the column, so
  the next collision is a clear message at the point of failure instead of a
  mystery in the settings UI. Add to `requiredColumns` when a migration adds
  something the code cannot function without.

## Conventions

- `NNNN_snake_case.sql`, starting with `-- +goose Up`.
- Say *why* in a comment at the top. Every migration here does.
- SQLite cannot alter a CHECK constraint, so changing one means the
  create-new → copy → drop-old → rename-new dance (see 0007, 0012, 0020).
  Never rename the old table first: `ALTER TABLE … RENAME` rewrites child FK
  references to follow it.
- Foreign keys are on (`_pragma=foreign_keys(1)`). A rebuild of a table with
  FK children needs that accounted for.
- Migrations that touch existing data get a test against the real prior
  schema — `goose.UpToContext(…, N-1)`, seed, then let yours run. See
  `rootkind_migration_test.go` and `migration0021_test.go`.
