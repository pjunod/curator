import { describe, expect, it } from 'vitest'
import { DOWNLOAD_PRIORITIES, downloadPriorityLabel } from './downloadPriority'

describe('download priorities', () => {
  it('matches nzbd scheduler bands', () => {
    expect(DOWNLOAD_PRIORITIES.map(({ value }) => value)).toEqual([-100, -50, 0, 50, 100, 900])
  })

  it('labels the common levels', () => {
    expect(downloadPriorityLabel(50)).toBe('High')
    expect(downloadPriorityLabel(900)).toBe('Force')
  })
})
