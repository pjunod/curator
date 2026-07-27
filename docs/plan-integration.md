# Integration plan — nzbd ↔ monarr ↔ plurx

**Written:** 2026-07-26 · **Recovered:** 2026-07-27

> **What this file is.** The original master plan lived here, untracked, and
> was lost. What follows is the surviving decision record — the confirmed
> decisions, the phase order, and the load-bearing facts each phase was built
> on. It is not the full master: the per-milestone detail for monarr's own
> packages (§5) is gone.
>
> The two per-repo plans **are** intact and are self-contained, including the
> contract slice each implements:
>
> - `nzbd/docs/INTEGRATION_PLAN.md` — N1–N7
> - `plurx/docs/INTEGRATION-PLAN.md` — P1–P6, plus P7/P8 sketched
>
> Anything below that conflicts with those two loses: they were written
> against the source and have been built from. This file is now tracked, so
> it cannot go missing again.

## The shape of it

Three applications behaving like one pipeline: events instead of timers at
every seam, and one transfer id from grab to playable.

## Decisions (confirmed 2026-07-26)

- **nzbd → monarr transport:** monarr subscribes to nzbd's SSE
  (`/api/v1/events`). nzbd gains **no outbound HTTP** and never learns
  monarr's address. Polling stays as the fallback; each client carries a
  `poll | push` mode.
  *Why:* nzbd already had SSE, and this keeps every piece of pairing config
  in monarr, where an operator is already looking.
- **plurx machine auth:** new scoped API keys (`plx_` bearer, scopes
  `scan:trigger` / `status:read`) — **not** an admin user.
  *Why:* an admin token can read the TMDB and Trakt secrets out of
  `/api/v1/settings`. A token IS a user; a key is not.
- **monarr → plurx:** `POST /api/v1/scan` with the path, the TMDB/IMDb ids,
  and a `correlation_id`. plurx's scan result feeds back into monarr's
  handoff trace.
- **Correlation id:** `t-<downloads.id>-<6 hex>`, carried as the nzbd job
  param `monarr-transfer` and as plurx's `correlation_id`.
- **Extras approved:** cross-app status panels, and nzbd capacity warnings
  surfaced in monarr. plurx's watched → monarr push and the coming-soon rail
  are **later** phases — re-scope before building.

## Phase order

| Phase | Work | State |
|---|---|---|
| 0 | Docs | done |
| 1 | nzbd — events, history cursor, add-time params (N1–N7) | done |
| 2 | monarr — native nzbd client + SSE subscriber | done |
| 3 | plurx — keys, targeted scan, scheduled reconcile (P1–P6) | done |
| 4 | monarr — import paths + the plurx notifier (§5.4–5.5) | done |
| 5 | Panels, metrics, docs | in progress |
| 6 | plurx watched → monarr; coming-soon rail | not started |

## Load-bearing facts (verified against source 2026-07-26)

Kept because each one is a thing that reads the opposite way from how it
behaves, and every phase built on them.

- **nzbd** `job_finished` fires at download-complete, **before**
  post-processing. PP finishing was silent — a history write plus
  `remove_job_silent`. `final_dir` exists only on history rows. N1 added the
  PP events that close this.
- **nzbd** `[[category]] dest_dir` was advertised to compat clients but never
  applied by post-processing. N6 fixed that honesty bug.
- **monarr** `ImportCompleted` carried no file paths or ids; notifier
  delivery had no retry and no log; `notifiers.type` is a SQLite CHECK, so
  adding `plurx` needed a table rebuild. §5.4–5.5 closed all three.
- **plurx** had **no scheduled scans at all**; a scan triggered during a
  running scan was dropped by `JobManager`; there was no path-targeted scan;
  NFO `<uniqueid>` is deliberately ignored; the config TOML is
  `deny_unknown_fields`, so new runtime knobs go in DB settings, not the
  file. P1–P6 closed these.

## How to work on this

Read the repo's own plan doc first — they are the authoritative,
self-contained ones. Re-verify every cited identifier against the source
before building: the quotes above were copied on 2026-07-26 and the code has
moved since.
