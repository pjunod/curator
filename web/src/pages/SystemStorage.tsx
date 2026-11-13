import { useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { CompletedFolders } from '../CompletedFolders'
import { ActionDialog, focusActionTrigger } from '../ActionDialog'
import { DeliveryLifecycleArchive, DeliveryLifecycleForDownload } from '../DeliveryLifecycle'
import { getDeliveryLifecycle, type ManualImportRequest } from '../api'
import { ManualImportPanel } from './Activity'

export function SystemStorage() {
  const qc = useQueryClient()
  const [manual, setManual] = useState<Partial<ManualImportRequest> | null>(null)
  const [delivery, setDelivery] = useState<number | null>(null)
  const opener = useRef<HTMLElement | null>(null)
  const heading = useRef<HTMLHeadingElement | null>(null)
  const history = useQuery({ queryKey: ['delivery-lifecycle', 'presence'], queryFn: () => getDeliveryLifecycle(undefined, 1) })
  const close = () => {
    if (!opener.current?.isConnected) opener.current = heading.current
    setManual(null); setDelivery(null)
    for (const key of ['activity', 'imports', 'completed-inventory']) void qc.invalidateQueries({ queryKey: [key] })
  }
  return <section id="storage">
    <h2 ref={heading} tabIndex={-1}>Storage</h2>
    <p className="muted">Completed files, storage identity and guarded cleanup. Ordinary downloads and imports are in Activity.</p>
    <button onClick={event => { focusActionTrigger(event); opener.current = document.activeElement instanceof HTMLElement ? document.activeElement : heading.current; setManual({}) }}>Import files…</button>
    <CompletedFolders onImport={(path, mode, downloadId, fingerprint) => {
      opener.current = document.activeElement instanceof HTMLElement ? document.activeElement : heading.current
      if (mode === 'delivery' && downloadId) setDelivery(downloadId)
      else setManual({ path, ...(mode === 'legacy' ? { reviewLegacy: true, reviewLegacyFingerprint: fingerprint } : {}) })
    }} />
    {history.error && <p role="alert">Historical delivery records are unavailable: {history.error.message} <button onClick={() => void history.refetch()}>Try again</button></p>}
    {!!history.data?.items.length && <DeliveryLifecycleArchive />}
    {manual && <ActionDialog title="Choose files to import" returnFocus={opener} onClose={close}><ManualImportPanel prefill={manual} onDone={close} /></ActionDialog>}
    {delivery !== null && <ActionDialog title="Inspect historical delivery files" returnFocus={opener} onClose={close}><DeliveryLifecycleForDownload downloadId={delivery} /></ActionDialog>}
  </section>
}
