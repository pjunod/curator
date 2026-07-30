import type { AutoSearchResult, AutoSearchTarget } from './api'

// describeAutoSearch turns an auto-search result into the one sentence the
// person who pressed the button actually wants.
//
// The old banner said "Searching in the background — grabs appear under
// Activity" every single time, because the endpoint returned 202 and had
// nothing else to say. That sentence is true of a search that grabbed a
// remux, a search that turned down everything it found, and a search that
// never ran — which is how a bug that made auto search a no-op for an item
// went unnoticed. The whole point of this function is that those three now
// read differently.
//
// The funnel is what makes the difference diagnosable: releases seen →
// releases that matched this target → releases the profile would accept.
// Whichever step went to zero first is the answer.
export function describeAutoSearch(res: AutoSearchResult): string {
  if (res.grabbed > 0) {
    const got = res.targets.filter((t) => t.grabbed)
    if (got.length === 1) return `Grabbed ${got[0].grabbed} — it's in Activity now.`
    return `Grabbed ${res.grabbed} releases — they're in Activity now.`
  }

  const errored = res.targets.filter((t) => t.error)
  if (errored.length > 0) {
    return `✕ ${errored[0].label}: ${errored[0].error}`
  }

  if (res.targets.length === 0) {
    return 'Nothing to search for — everything this item wants, it already has.'
  }

  const searched = res.targets.filter((t) => !t.skipped)
  if (searched.length === 0) {
    // Every target was skipped. Which reason is the useful part: one of them
    // means "turn monitoring on", the other means "wait, it's already going".
    const unmonitored = res.targets.filter((t) => t.skipped === 'unmonitored')
    if (unmonitored.length === res.targets.length) {
      return res.targets.length === 1
        ? 'Not searched — this is not monitored. Turn monitoring on and try again.'
        : `Not searched — none of the ${res.targets.length} targets are monitored.`
    }
    const downloading = res.targets.filter((t) => t.skipped === 'downloading')
    if (downloading.length === res.targets.length) {
      return 'Not searched — a download for this is already in progress. See Activity.'
    }
    return `Not searched: ${unmonitored.length} not monitored, ${downloading.length} already downloading.`
  }

  const seen = sum(searched, (t) => t.seen)
  const matched = sum(searched, (t) => t.matched)
  const accepted = sum(searched, (t) => t.accepted)
  const skipped = res.targets.length - searched.length
  const tail = skipped > 0 ? ` (${skipped} other target(s) skipped.)` : ''

  if (seen === 0) {
    return `No releases came back from any indexer for ${label(searched)}.${tail}`
  }
  if (matched === 0) {
    return `Saw ${seen} release(s), but none of them were ${label(searched)}.${tail}`
  }
  if (accepted === 0) {
    return `Found ${matched} matching release(s); the quality profile turned every one down. Interactive search shows why.${tail}`
  }
  // Accepted something and still grabbed nothing: the grab itself failed and
  // did not surface as an error. Rare, but silence here is what we're fixing.
  return `Accepted ${accepted} release(s) but grabbed none — check the logs.${tail}`
}

function sum(ts: AutoSearchTarget[], f: (t: AutoSearchTarget) => number): number {
  return ts.reduce((n, t) => n + f(t), 0)
}

// label names what was searched: the target itself when there is one, a count
// when there are several.
function label(ts: AutoSearchTarget[]): string {
  return ts.length === 1 ? ts[0].label : `any of the ${ts.length} targets searched`
}
