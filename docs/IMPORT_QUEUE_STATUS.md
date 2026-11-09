# Import Queue — implementation and delivery status

**Status:** implementation complete; adversarial review fixes applied; final suites running.
**Updated:** 2026-11-09.

Companion to [usage.md](usage.md#import-queue--follow-every-recorded-import):
this file records delivery evidence for the dedicated Import Queue page.

- [x] Isolated external-drive clone and `codex/import-queue` branch.
- [x] Server-side import index, complete counts, search and pagination.
- [x] Worker ownership, real progress, publication file outcomes and history.
- [x] Single and selected retries with individual visible results.
- [x] Existing cancellation, custody review and file resolution controls.
- [x] Desktop/sidebar, phone More and Activity navigation.
- [x] Adversarial review and findings addressed: publication completion, salvage,
  unattributed history, active custody labels and visible release identity.
- [x] Pinned lint: zero issues; web: 149 tests pass; production build passes.
- [x] New import API regressions pass after correcting fixture path persistence.
- [ ] Full Go and browser suites running; final results pending.
- [ ] Pull request, CI, merge and cleanup.

History covers retained download-backed imports, including dismissed Activity
records, plus read-only unattributed publications with actual imported or placed
file evidence. Ordinary retention still applies. Unimported files discovered
through Storage without any download attribution remain in Storage/Resolve. The new page does
not infer an import from the mere presence of a media file.
