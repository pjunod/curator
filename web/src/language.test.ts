import { describe, expect, it } from 'vitest'
import {
  audioUndeterminedNote,
  displayList,
  languageName,
  releaseLanguagesLabel,
  requiredAudioClause,
} from './language'

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

describe('audioUndeterminedNote', () => {
  it('says nothing when there is no requirement, nothing on disk, or the answer is known', () => {
    expect(audioUndeterminedNote(undefined, undefined, 'met')).toBeNull()
    expect(audioUndeterminedNote([], undefined, 'met')).toBeNull()
    expect(audioUndeterminedNote(['en'], undefined, 'missing')).toBeNull()
    expect(audioUndeterminedNote(['en'], undefined, '')).toBeNull()
    expect(audioUndeterminedNote(['en'], undefined, undefined)).toBeNull()
    // Known languages, met or not: the state word carries it already.
    expect(audioUndeterminedNote(['en'], ['en'], 'met')).toBeNull()
    expect(audioUndeterminedNote(['en'], ['de'], 'seeking')).toBeNull()
    expect(audioUndeterminedNote(['en'], [], 'seeking')).toBeNull()
  })

  it('flags a file whose tracks never declared a language, in every on-disk state', () => {
    for (const upgrade of ['met', 'seeking', 'capped']) {
      const note = audioUndeterminedNote(['en'], undefined, upgrade, options)
      expect(note?.word).toBe('audio language undetermined')
      expect(note?.why).toBe(
        'The profile requires English audio, but not every audio track on disk declares a language, so Curator cannot tell whether it is here. It will not replace a file it cannot judge; re-measure the files or check them yourself.',
      )
    }
  })

  it('names several required languages like the server does', () => {
    const note = audioUndeterminedNote(['en', 'fr'], undefined, 'met', options)
    expect(note?.why).toContain('requires English or French audio')
    // Without the server vocabulary the local map still names the common ones.
    expect(audioUndeterminedNote(['de'], undefined, 'met')?.why).toContain('requires German audio')
  })
})
