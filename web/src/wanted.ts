import type { WantedItem, WantedReason } from './api'

export type WantedFilter = 'all' | WantedReason
export type WantedSort = 'title' | 'reason' | 'kind'

export interface WantedGroup {
  mediaItemId: number
  title: string
  kind: WantedItem['kind']
  children: WantedItem[]
  missing: number
  upgrade: number
}

export function wantedReason(item: WantedItem): string {
  return item.reason === 'missing' ? 'missing' : `upgrade from ${item.current || 'current quality'}`
}

export function wantedKind(item: WantedItem): string {
  return item.kind
}

export function wantedText(item: WantedItem): string[] {
  return [
    item.title,
    item.detail,
    item.copy,
    item.kind,
    wantedReason(item),
    item.current,
    item.wantableId,
  ]
}

function compareText(a: string, b: string): number {
  return a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' })
}

export function compareWantedChildren(a: WantedItem, b: WantedItem): number {
  return (a.season ?? -1) - (b.season ?? -1) ||
    (a.episode ?? -1) - (b.episode ?? -1) ||
    a.copyId - b.copyId ||
    compareText(a.wantableId, b.wantableId)
}

export function groupWanted(items: WantedItem[]): WantedGroup[] {
  const groups = new Map<number, WantedGroup>()
  for (const item of items) {
    let group = groups.get(item.mediaItemId)
    if (!group) {
      group = {
        mediaItemId: item.mediaItemId,
        title: item.title,
        kind: item.kind,
        children: [],
        missing: 0,
        upgrade: 0,
      }
      groups.set(item.mediaItemId, group)
    }
    group.children.push(item)
    group[item.reason]++
  }
  for (const group of groups.values()) group.children.sort(compareWantedChildren)
  return [...groups.values()]
}

/**
 * Reason-filter children first, then retain whole groups on a text hit. Text
 * search never narrows siblings or a group action's target count.
 */
export function selectWantedGroups(
  items: WantedItem[],
  filter: WantedFilter,
  query: string,
  sort: WantedSort,
  direction: 'asc' | 'desc',
): WantedGroup[] {
  const reasonVisible = filter === 'all' ? items : items.filter((item) => item.reason === filter)
  const needle = query.trim().toLocaleLowerCase()
  const groups = groupWanted(reasonVisible).filter((group) => {
    if (!needle) return true
    if ([group.title, group.kind].some((value) => value.toLocaleLowerCase().includes(needle))) return true
    return group.children.some((item) => wantedText(item).some((value) => value.toLocaleLowerCase().includes(needle)))
  })

  return groups.sort((a, b) => {
    const left = sort === 'reason' ? (a.missing > 0 ? 'missing' : 'upgrade') : sort === 'kind' ? a.kind : a.title
    const right = sort === 'reason' ? (b.missing > 0 ? 'missing' : 'upgrade') : sort === 'kind' ? b.kind : b.title
    const primary = compareText(left, right)
    if (primary !== 0) return direction === 'asc' ? primary : -primary
    return compareText(a.title, b.title) || a.mediaItemId - b.mediaItemId
  })
}

// Retained for callers/tests that need a flat projection. New page behavior
// uses selectWantedGroups so pagination can never split one title.
export function selectWanted(
  items: WantedItem[],
  filter: WantedFilter,
  query: string,
  sort: WantedSort,
  direction: 'asc' | 'desc',
): WantedItem[] {
  return selectWantedGroups(items, filter, query, sort, direction).flatMap((group) => group.children)
}
