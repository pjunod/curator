import { useId, useLayoutEffect, useRef, type ReactNode, type SyntheticEvent } from 'react'

// Safari does not focus buttons on pointer activation. Remember the actual
// opener through focus before mounting a dialog so dismissal returns to it.
export function focusActionTrigger(event: SyntheticEvent<HTMLElement>) {
  event.currentTarget.focus({ preventScroll: true })
}

/** User-opened work belongs in the viewport, regardless of the trigger's position. */
export function ActionDialog({ title, className = '', onClose, children }: {
  title: string
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
      if (trigger?.isConnected) trigger.focus({ preventScroll: true })
    }
  }, [])

  return (
    <dialog ref={dialog} className="action-dialog" aria-labelledby={titleId}
      onCancel={(event) => { event.preventDefault(); onClose() }}>
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
export function ActionNotice({ children, warning = false }: { children: ReactNode; warning?: boolean }) {
  const notice = useRef<HTMLDivElement>(null)
  const previous = useRef<string | null>(null)
  useLayoutEffect(() => {
    const node = notice.current
    // Parent rerenders and background refetches must not steal the viewport.
    if (node && node.textContent !== previous.current) {
      previous.current = node.textContent
      node.scrollIntoView({ block: 'nearest', behavior: 'instant' })
    }
  })
  return <div ref={notice} className={`banner${warning ? ' warning' : ''}`} role="status">{children}</div>
}
