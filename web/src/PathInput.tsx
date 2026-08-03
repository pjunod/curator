import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { browseFilesystem } from './api'

/**
 * A path field that suggests directories as you type (ADR 0009 §2a).
 *
 * The server can see the filesystem and the browser cannot, so every
 * keystroke past a separator asks for that directory's children. Typing
 * `/media/` lists everything under it; typing `/media/Mo` narrows to the
 * folders starting with "Mo". Roots that are already registered are shown
 * greyed, because picking one only earns a duplicate error.
 */
export function PathInput({
  value,
  onChange,
  placeholder,
  id,
  showParent = false,
}: {
  value: string
  onChange: (v: string) => void
  placeholder?: string
  id?: string
  /** Show an explicit way back up when the field is being used as a browser. */
  showParent?: boolean
}) {
  const [open, setOpen] = useState(false)
  const [debounced, setDebounced] = useState(value)
  const wrapRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), 180)
    return () => clearTimeout(t)
  }, [value])

  // Clicking anywhere else closes the list; without this it lingers over
  // whatever the user moved on to.
  useEffect(() => {
    if (!open) return
    const onDocClick = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDocClick)
    return () => document.removeEventListener('mousedown', onDocClick)
  }, [open])

  const suggestions = useQuery({
    queryKey: ['filesystem', debounced],
    queryFn: () => browseFilesystem(debounced),
    enabled: open && debounced.startsWith('/'),
    // A path that does not exist yet is the normal case mid-typing, so a
    // failed lookup is not worth retrying or surfacing as an error.
    retry: false,
    staleTime: 30_000,
  })

  const dirs = suggestions.data?.dirs ?? []
  const canBrowseRoot = showParent && debounced !== '/'

  return (
    <div className="path-input" ref={wrapRef}>
      <input
        id={id}
        type="text"
        autoComplete="off"
        spellCheck={false}
        placeholder={placeholder ?? '/absolute/path/to/media'}
        value={value}
        onFocus={() => setOpen(true)}
        onChange={(e) => {
          onChange(e.target.value)
          setOpen(true)
        }}
        onKeyDown={(e) => {
          if (e.key === 'Escape') setOpen(false)
        }}
      />
      {open && (dirs.length > 0 || (showParent && suggestions.data?.parent) || canBrowseRoot) && (
        <ul className="path-suggestions">
          {showParent && suggestions.data?.parent && (
            <li>
              <button
                type="button"
                title={suggestions.data.parent}
                onClick={() => {
                  const parent = suggestions.data!.parent
                  onChange(parent === '/' ? '/' : parent + '/')
                  setOpen(true)
                }}
              >
                <span aria-hidden="true">↰</span> Parent folder
                <span className="muted path-suggestion-detail">{suggestions.data.parent}</span>
              </button>
            </li>
          )}
          {dirs.slice(0, 50).map((d) => (
            <li key={d.path}>
              <button
                type="button"
                className={d.registered ? 'taken' : undefined}
                title={d.registered ? 'Already a root folder' : d.path}
                onClick={() => {
                  // Keep the trailing separator so the next keystroke
                  // continues descending rather than filtering siblings.
                  onChange(d.path + '/')
                  setOpen(true)
                }}
              >
                {d.name}
                {d.registered && <span className="muted"> — already a root</span>}
              </button>
            </li>
          ))}
          {canBrowseRoot && (
            <li>
              <button
                type="button"
                title="Start at the server filesystem root"
                onClick={() => {
                  onChange('/')
                  setOpen(true)
                }}
              >
                Filesystem root <span className="muted path-suggestion-detail">/</span>
              </button>
            </li>
          )}
        </ul>
      )}
    </div>
  )
}
