import { useLayoutEffect } from 'react'

/** Fixed or sticky shell bars that cover page content when stuck. */
export const VIEWPORT_CHROME = '.mobile-top, .mobile-tabs, [data-layout="theater"] .sidebar'

/**
 * How much of the viewport's top and bottom edges the shell's bars cover.
 * Measures the actual bars, including safe-area padding and Theater's wrapped
 * tablet navigation, rather than guessing heights.
 */
export function viewportChromeInsets(): { top: number; bottom: number } {
  let top = 0
  let bottom = 0
  document.querySelectorAll<HTMLElement>(VIEWPORT_CHROME).forEach((bar) => {
    const style = getComputedStyle(bar)
    if (style.position !== 'fixed' && style.position !== 'sticky') return
    // Computed 'top' on fixed bottom tabs is a resolved pixel value, even
    // though the stylesheet specifies only bottom: 0.
    if (bar.matches('.mobile-tabs') && style.position === 'fixed') {
      bottom = Math.max(bottom, bar.offsetHeight + (parseFloat(style.bottom) || 0))
    } else {
      top = Math.max(top, bar.offsetHeight + (parseFloat(style.top) || 0))
    }
  })
  return { top, bottom }
}

/** The root scroll padding currently reserved for the shell's bars. */
export function rootScrollPadding(): { top: number; bottom: number } {
  const style = getComputedStyle(document.documentElement)
  return { top: parseFloat(style.scrollPaddingTop) || 0, bottom: parseFloat(style.scrollPaddingBottom) || 0 }
}

/**
 * Reserve the shell's bars as root scroll padding, so every browser-driven
 * reveal (keyboard focus, scrollIntoView, find-in-page) lands clear of the
 * opaque navigation instead of underneath it. `shell` changes whenever the
 * set of bars does (phone versus desktop chrome).
 */
export function useViewportChromeScrollPadding(shell: string) {
  useLayoutEffect(() => {
    const root = document.documentElement
    const apply = () => {
      const { top, bottom } = viewportChromeInsets()
      root.style.scrollPaddingTop = `${top}px`
      root.style.scrollPaddingBottom = `${bottom}px`
    }
    const resize = typeof ResizeObserver === 'undefined' ? undefined : new ResizeObserver(apply)
    const observeBars = () => {
      resize?.disconnect()
      document.querySelectorAll<HTMLElement>(VIEWPORT_CHROME).forEach((bar) => resize?.observe(bar))
      apply()
    }
    observeBars()
    // Theater is a root attribute; switching layouts swaps which bars count.
    const layout = typeof MutationObserver === 'undefined' ? undefined : new MutationObserver(observeBars)
    layout?.observe(root, { attributes: true, attributeFilter: ['data-layout'] })
    window.addEventListener('resize', apply)
    return () => {
      resize?.disconnect()
      layout?.disconnect()
      window.removeEventListener('resize', apply)
      root.style.scrollPaddingTop = ''
      root.style.scrollPaddingBottom = ''
    }
  }, [shell])
}
