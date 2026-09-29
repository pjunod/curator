// The client-side mirror of the server's Profile.Sentence (ADR 0014, 0022).
//
// The server remains the authority — every saved profile renders the sentence
// the server sent — but the editor shows what a draft WILL say before it is
// saved, so the clauses and their order here must match quality.go exactly:
// target · floor · audio language · upgrades off. A preview that puts the
// clauses in a different order from the row it becomes is a preview that
// lies, quietly, about a thing the user is about to commit.

import type { LanguageOption } from './api'
import { requiredAudioClause } from './language'

export interface SentenceParts {
  /** The target, already rendered: "WEB-DL 1080p", "EPUB". */
  target: string
  /** The floor, already rendered; absent when none is set. */
  floor?: string
  upgradesAllowed: boolean
  /** Required audio language codes; empty for no requirement. */
  audioLanguages: string[]
}

export function buildProfileSentence(p: SentenceParts, languages?: LanguageOption[]): string {
  let s = `hunts the best release up to ${p.target}, then stops`
  if (p.floor) s += `; never below ${p.floor}`
  s += requiredAudioClause(p.audioLanguages, languages)
  if (!p.upgradesAllowed) s += '; no upgrades once a file is present'
  return s
}
