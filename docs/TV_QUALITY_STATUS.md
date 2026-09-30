# TV quality details — delivery status

Companion to [usage.md](usage.md#reading-the-quality-row), which explains
the quality display. This page tracks the compact TV quality change.

**Updated:** 2026-09-29 · **Version:** 0.34.2 ·
**Branch:** `codex/compact-tv-quality`

| Work | Status | Evidence |
|---|---|---|
| Isolated workspace | Complete | Independent clone of GitHub main at `5f3ec3c`, outside `~/code`; original checkout restored. |
| Compact quality row | Complete | Multiple measured TV files use a collapsed disclosure; quality and upgrade status remain visible. |
| Detailed measurements | Complete | Identical measurements share a row with a file count; the list scrolls at 14 rem. Primary-copy files only; counts are files, not episodes. |
| Single-file and movie display | Preserved | Measurements remain inline. |
| Feature availability | Complete | Enabled directly, without a feature flag or settings gate. |
| Usage documentation | Complete | Quality-row guide updated. |
| Adversarial review | Approved | Independent review of `1256bd3`: no actionable findings. Checked filtering, counts, grouping, scrolling, keyboard access, responsive styles, and quality semantics; no tests run during review. |
| Final validation and merge | Tracked in delivery PR | The linked PR's checks and merge state are the live result; final CI runs after review, without duplicate local unit runs. |

## Decisions

- Use an expandable measurements list rather than hiding or truncating the
  underlying facts. The Files table retains every file's measurements.
- Bump to 0.34.2 because GitHub main already contains 0.34.1.
- Keep implementation, documentation, and review fixes together in one PR.
- Follow the user's isolated-clone and final-test workflow over the older
  instructions in `CLAUDE.md` to edit the user's checkout and repeat tests.

The [delivery PR](https://github.com/pjunod/curator/pulls?q=is%3Apr+head%3Acodex%2Fcompact-tv-quality)
is the live authority for final check results and merge state.
