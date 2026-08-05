import { describe, expect, it } from 'vitest'
import { compatibleRootFolders, resolveRootFolderID } from './addMediaOptions'
import type { RootFolder } from './types'

const root = (id: number, kind: RootFolder['kind']): RootFolder => ({
  id,
  kind,
  path: `/media/${id}`,
  freeBytes: 1,
  accessible: true,
})

describe('native add-media root selection', () => {
  const roots = [root(1, 'series'), root(2, 'movie'), root(3, 'mixed')]

  it('offers only roots that accept the selected media kind', () => {
    expect(compatibleRootFolders(roots, 'movie').map((candidate) => candidate.id)).toEqual([2, 3])
  })

  it('preselects the first compatible root instead of an undefined server default', () => {
    expect(resolveRootFolderID(0, compatibleRootFolders(roots, 'movie'))).toBe(2)
  })

  it('keeps an explicit compatible selection and reports none when no root matches', () => {
    expect(resolveRootFolderID(3, compatibleRootFolders(roots, 'movie'))).toBe(3)
    expect(resolveRootFolderID(0, compatibleRootFolders([root(1, 'series')], 'book'))).toBe(0)
  })
})
