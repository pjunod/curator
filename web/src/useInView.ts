import { useEffect, useRef, useState } from 'react'

// How far ahead of the viewport a row starts loading. Wide enough that the
// fetch is already in flight by the time the row is scrolled to, so lazy
// loading is invisible rather than a stutter.
const ROOT_MARGIN = '400px'

/** useInView reports whether the returned ref has come within ROOT_MARGIN of
 *  the viewport, and stays true once it has.
 *
 *  It latches deliberately: the point is to defer the first fetch, not to
 *  unload a row that scrolls away. A row that emptied itself on scroll-out
 *  would refetch on every pass up and down the page, which is more upstream
 *  traffic than fetching everything eagerly would have cost.
 *
 *  Environments without IntersectionObserver (older browsers, a test runner
 *  with no DOM) get true immediately — degrading to eager loading, which is
 *  correct, just less thrifty. */
export function useInView<T extends HTMLElement>() {
  const ref = useRef<T | null>(null)
  const [seen, setSeen] = useState(typeof IntersectionObserver === 'undefined')

  useEffect(() => {
    if (seen || !ref.current) return
    const el = ref.current
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          setSeen(true)
          io.disconnect()
        }
      },
      { rootMargin: ROOT_MARGIN },
    )
    io.observe(el)
    return () => io.disconnect()
  }, [seen])

  return { ref, seen }
}
