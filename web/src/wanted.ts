import type { WantedItem } from './api'

export type WantedFilter = 'all' | 'missing' | 'upgrade'
export type WantedSort = 'title' | 'reason' | 'kind'

/** The reason text is deliberately shared by the row and the text search. */
export function wantedReason(item: WantedItem): string {
  return item.missing ? 'missing' : `upgrade from ${item.current || 'current quality'}`
}

export function wantedKind(item: WantedItem): string {
  const kind = item.wantableId.split(':', 1)[0]
  if (kind === 'episode' || kind === 'season') return 'series'
  return kind
}

/**
 * Apply all wanted-list controls in one place so filtering, searching, counts,
 * and pagination cannot quietly disagree about which rows are visible.
 */
export function selectWanted(
  items: WantedItem[],
  filter: WantedFilter,
  query: string,
  sort: WantedSort,
  direction: 'asc' | 'desc',
): WantedItem[] {
  const needle = query.trim().toLocaleLowerCase()
  const selected = items.filter((item) => {
    if (filter === 'missing' && !item.missing) return false
    if (filter === 'upgrade' && item.missing) return false
    if (!needle) return true

    // Searching for "missing" or "upgrade" is useful in its own right, while
    // the remaining fields make this a normal title/detail/copy search too.
    return [
      item.title,
      item.detail,
      item.copy,
      wantedKind(item),
      wantedReason(item),
      item.current,
      item.wantableId,
    ].some((value) => value.toLocaleLowerCase().includes(needle))
  })

  const compare = (a: WantedItem, b: WantedItem): number => {
    const left = sort === 'reason' ? wantedReason(a) : sort === 'kind' ? wantedKind(a) : a.title
    const right = sort === 'reason' ? wantedReason(b) : sort === 'kind' ? wantedKind(b) : b.title
    const primary = left.localeCompare(right, undefined, { numeric: true, sensitivity: 'base' })
    if (primary !== 0) return direction === 'asc' ? primary : -primary
    return a.title.localeCompare(b.title, undefined, { numeric: true, sensitivity: 'base' })
  }

  return [...selected].sort(compare)
}
