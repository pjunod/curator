/**
 * Shared pagination controls: a page-size chooser and page buttons.
 *
 * One component so the library grid and the adoption review window page the
 * same way — two pagers that disagree about what "next" does is worse than
 * either alone.
 */

/** Page-size choices. 0 means "All", which is the escape hatch for anyone
 *  who would rather scroll or use the browser's own find. */
export const PAGE_SIZES = [25, 50, 100, 200, 500, 0] as const

export function pageSizeLabel(n: number): string {
  return n === 0 ? 'All' : String(n)
}

/**
 * clampPage keeps a page number inside a list that may have shrunk under
 * it — dismissing the last entry on page 12 should land you on the new last
 * page, not on an empty one.
 */
export function clampPage(page: number, total: number, size: number): number {
  if (size === 0) return 0
  const pages = Math.max(1, Math.ceil(total / size))
  return Math.min(Math.max(0, page), pages - 1)
}

/** sliceForPage applies a page window, treating size 0 as "everything". */
export function sliceForPage<T>(items: T[], page: number, size: number): T[] {
  if (size === 0) return items
  const start = clampPage(page, items.length, size) * size
  return items.slice(start, start + size)
}

export function PageSizePicker({
  size,
  onChange,
  label = 'Per page',
}: {
  size: number
  onChange: (n: number) => void
  label?: string
}) {
  return (
    <label className="page-size">
      <span className="muted">{label}</span>
      <select value={size} onChange={(e) => onChange(Number(e.target.value))}>
        {PAGE_SIZES.map((n) => (
          <option key={n} value={n}>
            {pageSizeLabel(n)}
          </option>
        ))}
      </select>
    </label>
  )
}

export function Pager({
  page,
  size,
  total,
  onPage,
}: {
  page: number
  size: number
  total: number
  onPage: (n: number) => void
}) {
  // With everything on one page there is nothing to navigate, and an
  // always-visible dead pager is just furniture.
  if (size === 0 || total <= size) {
    return (
      <span className="muted">
        {total === 0 ? 'Nothing to show' : `${total} shown`}
      </span>
    )
  }
  const pages = Math.max(1, Math.ceil(total / size))
  const current = clampPage(page, total, size)
  const from = current * size + 1
  const to = Math.min((current + 1) * size, total)

  return (
    <>
      <span className="muted">
        {from}–{to} of {total}
      </span>
      <div className="pager">
        <button disabled={current === 0} onClick={() => onPage(0)} title="First page">
          «
        </button>
        <button disabled={current === 0} onClick={() => onPage(current - 1)}>
          ‹ Prev
        </button>
        <span className="muted">
          Page {current + 1} of {pages}
        </span>
        <button disabled={current + 1 >= pages} onClick={() => onPage(current + 1)}>
          Next ›
        </button>
        <button
          disabled={current + 1 >= pages}
          onClick={() => onPage(pages - 1)}
          title="Last page"
        >
          »
        </button>
      </div>
    </>
  )
}
