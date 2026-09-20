import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { deleteLibraryItem, getScanReport, repairLibraryFolder } from './api'
import type { MissingItem } from './api'
import { PathInput } from './PathInput'
import { Pager, sliceForPage } from './Pager'

const reasons = {
  missing: 'Folder not found',
  inaccessible: 'Folder cannot be accessed — check the drive, mount, or permissions',
  not_directory: 'The saved path is a file, not a folder',
}

export function MissingFolders({ items }: { items: MissingItem[] }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [page, setPage] = useState(0)
  const [paths, setPaths] = useState<Record<number, string>>({})
  const [errors, setErrors] = useState<Record<number, string>>({})
  const [message, setMessage] = useState('')

  const refresh = async () => {
    await Promise.all([
      qc.invalidateQueries({ queryKey: ['scan-report'] }),
      qc.invalidateQueries({ queryKey: ['library'] }),
      qc.invalidateQueries({ queryKey: ['library-item'] }),
      qc.invalidateQueries({ queryKey: ['review'] }),
      qc.invalidateQueries({ queryKey: ['wanted'] }),
    ])
  }
  const repair = useMutation({
    mutationFn: async () => {
      let repaired = 0
      const failures: Record<number, string> = {}
      // Apply all entered paths, including other pages. Continue after a
      // failed row so one unavailable drive does not block other repairs.
      for (const item of items) {
        const path = paths[item.id]?.trim()
        if (!path) continue
        try {
          await repairLibraryFolder(item.id, item.path, path)
          repaired++
          setPaths((current) => {
            const next = { ...current }
            delete next[item.id]
            return next
          })
        } catch (err) {
          failures[item.id] = (err as Error).message
        }
      }
      setErrors(failures)
      setMessage(`${repaired} folder${repaired === 1 ? '' : 's'} repaired` +
        (Object.keys(failures).length ? ` · ${Object.keys(failures).length} still need attention` : '.'))
      await refresh()
    },
  })
  const recheck = useMutation({
    mutationFn: async () => {
      const report = await getScanReport()
      qc.setQueryData(['scan-report'], report)
      setMessage(`${report?.missingItems?.length ?? 0} folder issues remain.`)
    },
  })
  const forget = useMutation({ mutationFn: deleteLibraryItem, onSuccess: refresh })
  const busy = repair.isPending || recheck.isPending || forget.isPending
  const selected = items.filter((item) => paths[item.id]?.trim()).length

  return (
    <div className="missing-folders">
      {message && <p role="status">{message}</p>}
      {items.length > 0 && (
        <details>
          <summary>Folders needing attention ({items.length})</summary>
          <p className="muted">
            These entries have recorded media files, but their saved folder is missing or
            cannot be accessed. Titles awaiting their first download are not listed.
            Restore the drive or mount and check again, or choose the folder containing each
            title’s files. An empty folder will not recover missing media.
          </p>
          <div className="form-row">
            <button onClick={() => setEditing(!editing)} disabled={busy}>
              {editing ? 'Close repair fields' : 'Repair folders…'}
            </button>
            <button onClick={() => recheck.mutate()} disabled={busy}>
              {recheck.isPending ? 'Checking…' : 'Check again'}
            </button>
            {editing && (
              <button className="btn-accent" disabled={busy || selected === 0} onClick={() => repair.mutate()}>
                {repair.isPending ? 'Repairing…' : `Apply all repairs (${selected})`}
              </button>
            )}
          </div>
          {editing && (
            <p className="muted">
              Choose existing folders under a registered library root, then apply all entered
              paths together. Files stay where they are. If the files were deleted, restore
              them from backup or open the title to search for a replacement.
            </p>
          )}
          <div className="form-row">
            <Pager page={page} size={25} total={items.length} onPage={setPage} />
          </div>
          <fieldset disabled={busy} className="folder-repair-fields">
            {sliceForPage(items, page, 25).map((item) => (
              <div className="folder-repair-row" key={item.id}>
                <div className="form-row">
                  <Link to="/library/$id" params={{ id: String(item.id) }}>{item.title}</Link>
                  <span className="muted">{reasons[item.reason ?? 'missing']}</span>
                  <button
                    className="link-btn"
                    title="Remove this library entry; files on disk are untouched"
                    onClick={() => forget.mutate(item.id)}
                  >Remove entry</button>
                </div>
                <code className="folder-repair-path">{item.path}</code>
                {editing && (
                  <>
                    <label htmlFor={`repair-folder-${item.id}`}>Existing folder for {item.title}</label>
                    <PathInput
                      id={`repair-folder-${item.id}`}
                      value={paths[item.id] ?? ''}
                      placeholder={item.path}
                      showParent
                      onChange={(path) => setPaths((current) => ({ ...current, [item.id]: path }))}
                    />
                  </>
                )}
                {errors[item.id] && <p className="error-text" role="alert">{errors[item.id]}</p>}
              </div>
            ))}
          </fieldset>
        </details>
      )}
      {(repair.error || recheck.error || forget.error) && (
        <p className="error-text" role="alert">{String((repair.error || recheck.error || forget.error)?.message)}</p>
      )}
    </div>
  )
}
