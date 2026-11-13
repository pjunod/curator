import { useId, useLayoutEffect, useRef, type ReactNode, type SyntheticEvent, type RefObject } from 'react'
import { rootScrollPadding, viewportChromeInsets } from './viewportChrome'

// Safari does not focus buttons on pointer activation. Remember the actual
// opener through focus before mounting a dialog so dismissal returns to it.
export function focusActionTrigger(event: SyntheticEvent<HTMLElement>) {
  event.currentTarget.focus({ preventScroll: true })
}

/** User-opened work belongs in the viewport, regardless of the trigger's position. */
export function ActionDialog({ title, className = '', onClose, children, returnFocus }: {
  title: string
  returnFocus?: RefObject<HTMLElement | null>
  className?: string
  onClose: () => void
  children: ReactNode
}) {
  const dialog = useRef<HTMLDialogElement>(null)
  const heading = useRef<HTMLHeadingElement>(null)
  const titleId = useId()

  useLayoutEffect(() => {
    const node = dialog.current!
    const trigger = document.activeElement instanceof HTMLElement ? document.activeElement : null
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    node.showModal()
    // Start at the title, without opening the phone's on-screen keyboard.
    heading.current?.focus({ preventScroll: true })
    return () => {
      node.close()
      document.body.style.overflow = previousOverflow
      const target = returnFocus?.current?.isConnected ? returnFocus.current : trigger
      if (target?.isConnected) target.focus({ preventScroll: true })
    }
  }, [returnFocus])

  return (
    <dialog ref={dialog} className="action-dialog" aria-labelledby={titleId}
      onCancel={(event) => { event.preventDefault(); event.stopPropagation(); onClose() }}>
      <section className={`panel ${className}`}>
        <header className="action-dialog-head">
          <h2 id={titleId} ref={heading} tabIndex={-1}>{title}</h2>
          <button onClick={onClose}>Close</button>
        </header>
        <div className="action-dialog-body">{children}</div>
      </section>
    </dialog>
  )
}

/** Reveal new action feedback even when its initiating row was far below it. */
export function ActionNotice({ children, warning = false, revision = 0 }: {
  children: ReactNode
  warning?: boolean
  revision?: number
}) {
  const notice = useRef<HTMLDivElement>(null)
  const previous = useRef<{ text: string | null; revision: number } | null>(null)
  useLayoutEffect(() => {
    const node = notice.current
    // A new result can repeat the same text. Ordinary rerenders cannot.
    if (node && (node.textContent !== previous.current?.text || revision !== previous.current?.revision)) {
      previous.current = { text: node.textContent, revision }
      if (!node.closest('dialog')) {
        // Page feedback shares the viewport with navigation; native dialogs
        // sit above it. The shell normally reserves its bars as root scroll
        // padding; scroll margin adds breathing room plus any bar height that
        // padding does not already cover, so the two never count it twice.
        const bars = viewportChromeInsets()
        const reserved = rootScrollPadding()
        node.style.scrollMarginTop = `${Math.max(16, bars.top + 16 - reserved.top)}px`
        node.style.scrollMarginBottom = `${Math.max(16, bars.bottom + 16 - reserved.bottom)}px`
      }
      node.scrollIntoView({ block: 'nearest', behavior: 'instant' })
    }
  })
  return <div ref={notice} className={`banner${warning ? ' warning' : ''}`} role="status">{children}</div>
}
