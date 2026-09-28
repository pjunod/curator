// Audio-language display helpers (ADR 0022).
//
// The server owns the vocabulary — GET /languages is the list a profile may
// require, and every stored code is canonical ISO 639-1 — so nothing here
// decides what a language IS. What lives here is the rendering: turning codes
// into names for a table cell, and turning a list of names into the same
// clause the server's Profile.Sentence renders, so the editor's live preview
// says exactly what the saved row will say.

import type { LanguageOption } from './api'

/** The canonical code for "several languages, unnamed" — a MULTi/DUAL release. */
export const MULTI = 'mul'

// A small fallback map so a release row can name the common cases before (or
// without) the /languages response. Anything not here shows as its code,
// uppercased — the server's Display does the same, so nothing is hidden.
const FALLBACK_NAMES: Record<string, string> = {
  en: 'English',
  de: 'German',
  fr: 'French',
  es: 'Spanish',
  it: 'Italian',
  pt: 'Portuguese',
  nl: 'Dutch',
  ja: 'Japanese',
  ko: 'Korean',
  zh: 'Chinese',
  ru: 'Russian',
  pl: 'Polish',
  sv: 'Swedish',
  hi: 'Hindi',
}

/** languageName renders one code the way people write it: "de" → "German",
 *  "mul" → "MULTi", anything unknown → the code uppercased. */
export function languageName(code: string, options?: LanguageOption[]): string {
  if (code === MULTI) return 'MULTi'
  const known = options?.find((o) => o.code === code)
  if (known) return known.name
  return FALLBACK_NAMES[code] ?? code.toUpperCase()
}

/** displayList joins names for a sentence the way the server does:
 *  "English", "English or French", "English, French or German". */
export function displayList(names: string[]): string {
  if (names.length === 0) return ''
  if (names.length === 1) return names[0]
  return `${names.slice(0, -1).join(', ')} or ${names[names.length - 1]}`
}

/**
 * requiredAudioClause is the sentence fragment a profile with a language
 * requirement gains — "; English audio required" — or '' when it has none.
 * Codes are sorted first because the server normalizes a stored list that way
 * (language.Normalize), and the preview must not say "French or English" for a
 * row that will read "English or French".
 */
export function requiredAudioClause(codes: string[], options?: LanguageOption[]): string {
  const sorted = Array.from(new Set(codes)).sort()
  if (sorted.length === 0) return ''
  return `; ${displayList(sorted.map((c) => languageName(c, options)))} audio required`
}

/**
 * releaseLanguagesLabel is what an interactive-search row says beside its
 * quality. A name that says nothing reads as English (the scene convention),
 * and that is the ordinary case — so exactly ["en"] renders nothing, and only
 * a release that advertises something else earns a label.
 */
export function releaseLanguagesLabel(codes: string[] | undefined, options?: LanguageOption[]): string {
  if (!codes || codes.length === 0) return ''
  if (codes.length === 1 && codes[0] === 'en') return ''
  return codes.map((c) => languageName(c, options)).join(' + ')
}
