import { describe, expect, it } from 'vitest'
import { mapReplacementObservationsByFileID } from './replacementObservations'

describe('replacement observation association', () => {
  it('keeps stale results attached to file IDs when scope order differs from numeric object-key order', () => {
    const ids = { 2: 'observation-old', 19: 'observation-new' }
    const entries = Object.entries(ids)
    const queryResults = [
      { observationId: 'observation-old', state: 'ready' },
      { observationId: 'observation-new', state: 'stale' },
    ]
    const byFileID = mapReplacementObservationsByFileID(entries, queryResults)
    const scopeFiles = [{ id: 19 }, { id: 2 }]

    expect(scopeFiles.map(({ id }) => byFileID.get(id)?.state)).toEqual(['stale', 'ready'])
  })
})
