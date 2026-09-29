import { describe, expect, it } from 'vitest'
import { buildProfileSentence } from './profileSentence'

const options = [
  { code: 'en', name: 'English' },
  { code: 'fr', name: 'French' },
]

describe('buildProfileSentence', () => {
  it('renders the bare target the way the server does', () => {
    expect(buildProfileSentence({ target: 'WEB-DL 1080p', upgradesAllowed: true, audioLanguages: [] })).toBe(
      'hunts the best release up to WEB-DL 1080p, then stops',
    )
  })

  it('orders every clause exactly as quality.go Profile.Sentence does', () => {
    // target · floor · audio language · upgrades off — the language clause
    // sits BEFORE the upgrades-off clause, not after it.
    expect(
      buildProfileSentence(
        { target: 'WEB-DL 1080p', floor: 'HDTV 1080p', upgradesAllowed: false, audioLanguages: ['en'] },
        options,
      ),
    ).toBe(
      'hunts the best release up to WEB-DL 1080p, then stops; never below HDTV 1080p; English audio required; no upgrades once a file is present',
    )
  })

  it('names several languages the way the server list does', () => {
    expect(
      buildProfileSentence(
        { target: 'Bluray 2160p', upgradesAllowed: true, audioLanguages: ['fr', 'en'] },
        options,
      ),
    ).toBe('hunts the best release up to Bluray 2160p, then stops; English or French audio required')
  })
})
