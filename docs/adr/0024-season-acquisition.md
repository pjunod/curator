# ADR 0024 — saturated episode outcomes and season acquisition

**Status:** proposed · **Date:** 2026-10-01

Companion to [the implementation contract](../plan-season-acquisition.md).

## Decision

Compare complete season/copy discovery before automatic dispatch. Maximize
attainable eligible outcomes per episode, saturating each at the profile target.
Evaluate no pack and every uniform full-season pack with independently useful
singles. Among equivalent outcomes minimize known-zero torrent risk, unpriced
transfers, known bytes, unknown torrent health, redundant payload and transfers.
Source/format preferences only break final ties.

Protect existing files and selected better-single slots with immutable import
allowlists. Measured live policy precedes publication. Persist one execution
plan and existing download intents; uncertainty is a reservation, not rejection.
Bound broad claims while retaining affected episode custody. Use a shared
request ledger and checkpointed queue to finish feasible discovery comparisons.

## Consequences

The optimizer cannot trade one episode's quality against another's. Mixed or
partial pack payload shapes are excluded. Full pack bytes are charged even when
protected files are skipped. Singles during genuine incomplete discovery can
later duplicate pack bytes; that availability tradeoff is explicit. Seven-day
uncertainty retry accepts possible duplicates and records a persistent warning.
One acquisition/import process remains required; deployment is excluded.

## Failed discovery recovery — 2026-11-13

Optimistic snapshot/custody races are scheduling deferrals: clear discovery
evidence and compare again after 30 seconds without exhausting the failure
budget. The admission transaction still checks the library revision.

The season coordinator also recovers exhausted searches when an aired,
monitored episode remains missing and unreserved. It retries after five
minutes, doubling to a one-hour ceiling on repeated failures. Every recovery
uses a fresh background checkpoint; an atomic check against the newest job
prevents superseding a concurrent request. Completed comparisons, provider
budgets, offloads, monitoring and re-grab caps retain their existing rules.
