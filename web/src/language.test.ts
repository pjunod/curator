import { describe, expect, it } from 'vitest'
import { displayList, languageName, releaseLanguagesLabel, requiredAudioClause } from './language'

const options = [
  { code: 'en', name: 'English' },
  { code: 'de', name: 'German' },
  { code: 'fr', name: 'French' },
  { code: 'et', name: 'Estonian' },
]

describe('displayList', () => {
  it('joins names the way the server sentence does', () => {
    expect(displayList([])).toBe('')
    expect(displayList(['English'])).toBe('English')
    expect(displayList(['English', 'French'])).toBe('English or French')
    expect(displayList(['English', 'French', 'German'])).toBe('English, French or German')
  })
})

describe('languageName', () => {
  it('prefers the server vocabulary, then the local map, then the code', () => {
    expect(languageName('et', options)).toBe('Estonian')
    expect(languageName('de')).toBe('German')
    expect(languageName('xx')).toBe('XX')
    expect(languageName('mul')).toBe('MULTi')
  })
})

describe('requiredAudioClause', () => {
  it('is empty with no requirement', () => {
    expect(requiredAudioClause([], options)).toBe('')
  })

  it('renders the clause the saved row will carry, in the stored order', () => {
    expect(requiredAudioClause(['en'], options)).toBe('; English audio required')
    // The server sorts codes on save, so the preview sorts too: "fr, en"
    // stored is "en, fr", which reads "English or French".
    expect(requiredAudioClause(['fr', 'en'], options)).toBe('; English or French audio required')
    // Sorted by code (de, en, fr), deduped.
    expect(requiredAudioClause(['fr', 'en', 'de', 'en'], options)).toBe(
      '; German, English or French audio required',
    )
  })
})

describe('releaseLanguagesLabel', () => {
  it('says nothing for the ordinary English-only release', () => {
    expect(releaseLanguagesLabel(undefined)).toBe('')
    expect(releaseLanguagesLabel([])).toBe('')
    expect(releaseLanguagesLabel(['en'])).toBe('')
  })

  it('names everything else', () => {
    expect(releaseLanguagesLabel(['de'], options)).toBe('German')
    expect(releaseLanguagesLabel(['en', 'de'], options)).toBe('English + German')
    expect(releaseLanguagesLabel(['mul'])).toBe('MULTi')
  })
})
