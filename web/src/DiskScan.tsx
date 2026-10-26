import { useEffect, useState } from 'react'
import { ActionDialog, focusActionTrigger } from './ActionDialog'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getScanReport, getScanStatus, triggerScan, type ScanReport, type ScanStatus } from './api'

export function useDiskScan() {
  const qc = useQueryClient()
  const status = useQuery({
    queryKey: ['disk-scan'],
    queryFn: getScanStatus,
    refetchInterval: 2_000,
  })
  const reportMatchesScan = (candidate: ScanReport | null | undefined) =>
    !!candidate && !!status.data?.requestedAt &&
    Date.parse(candidate.scannedAt) >= Date.parse(status.data.requestedAt)
  const report = useQuery({
    queryKey: ['scan-report'],
    queryFn: getScanReport,
    // A failed refresh retains old data in React Query. Keep trying without
    // presenting that older report as the newly completed scan's results.
    refetchInterval: (query) => status.data?.state === 'completed' && !reportMatchesScan(query.state.data) ? 2_000 : false,
  })
  const request = useMutation({
    mutationFn: triggerScan,
    onMutate: async () => {
      // An older GET must not overwrite the job returned by this request.
      await qc.cancelQueries({ queryKey: ['disk-scan'] })
      const previous = qc.getQueryData<ScanStatus>(['disk-scan'])
      return { previous }
    },
    onSuccess: (scan) => {
      qc.setQueryData(['disk-scan'], scan)
      // Inline scans have no job ID to distinguish consecutive completions.
      if (scan.state === 'completed' && !scan.id) {
        void qc.invalidateQueries({ queryKey: ['scan-report'] })
        void qc.invalidateQueries({ queryKey: ['review'] })
        void qc.invalidateQueries({ queryKey: ['library'] })
      }
    },
    onSettled: () => { void qc.invalidateQueries({ queryKey: ['disk-scan'] }) },
  })

  const id = status.data?.id
  const state = status.data?.state
  useEffect(() => {
    if (state !== 'completed' && state !== 'failed') return
    void qc.invalidateQueries({ queryKey: ['scan-report'] })
    void qc.invalidateQueries({ queryKey: ['review'] })
    void qc.invalidateQueries({ queryKey: ['library'] })
  }, [qc, id, state, status.data?.requestedAt])

  const previous = request.context?.previous
  const observedNewJob = !!previous && !!status.data && status.data.state !== 'idle' &&
    (status.data.id !== previous?.id || status.data.requestedAt !== previous?.requestedAt)
  const observedActiveJob = state === 'queued' || state === 'running'
  useEffect(() => {
    // Once durable work confirms an ambiguous request, do not resurrect its
    // transport error when that job later reaches a terminal state.
    if (request.error && (observedNewJob || observedActiveJob)) request.reset()
  }, [request.error, request.reset, observedNewJob, observedActiveJob])

  return {
    start: () => request.mutate(),
    busy: request.isPending || state === 'queued' || state === 'running',
    requesting: request.isPending,
    status: status.data,
    report: reportMatchesScan(report.data) ? report.data : undefined,
    reportError: report.error,
    requestError: observedNewJob || observedActiveJob ? null : request.error,
    statusError: status.error,
  }
}

export function DiskScanFeedback({ scan }: { scan: ReturnType<typeof useDiskScan> }) {
  if (scan.requestError) {
    return <div className="banner warning" role="alert">Could not start disk scan: {scan.requestError.message}</div>
  }
  let message = ''
  if (scan.requesting) message = 'Requesting disk scan…'
  else if (scan.status?.state === 'queued') {
    const attempts = scan.status.attempts ?? 0
    message = scan.status.error
      ? `Disk scan waiting to retry after ${attempts} ${attempts === 1 ? 'attempt' : 'attempts'}: ${scan.status.error}`
      : 'Disk scan queued — waiting for a worker…'
  } else if (scan.status?.state === 'running') message = 'Scanning disk… Results will refresh when the scan finishes.'
  else if (scan.status?.state === 'failed') message = `Disk scan failed: ${scan.status.error || 'Unknown error'}`
  else if (scan.status?.state === 'completed') {
    const report = scan.report
    message = report
      ? `Disk scan complete. ${report.itemsScanned} items scanned · ${report.filesLinked} files linked · ${report.filesRemoved} removed.`
      : scan.reportError
        ? `Disk scan complete. Could not load scan results: ${scan.reportError.message}. Retrying…`
        : 'Disk scan complete. Loading results…'
  }

  const failed = scan.status?.state === 'failed' || (scan.status?.state === 'completed' && !!scan.reportError)
  return <>
    {message && <div aria-label="Disk scan status" className={`banner${failed ? ' warning' : ''}`} role={failed ? 'alert' : 'status'}>{message}</div>}
    {scan.statusError && <div className="banner warning" role="alert">Could not check disk scan status: {scan.statusError.message}. Retrying…</div>}
  </>
}

// Keep user-opened progress in the viewport, including Settings controls far
// down the page. Polling only updates this dialog; it never scrolls or focuses.
export function DiskScanControl({ label }: { label: string }) {
  const scan = useDiskScan()
  const [open, setOpen] = useState(false)
  return <>
    <button onClick={(event) => {
      focusActionTrigger(event)
      setOpen(true)
      if (!scan.busy) scan.start()
    }}>
      {scan.busy ? 'Scanning…' : label}
    </button>
    {open && <ActionDialog title="Disk scan" onClose={() => setOpen(false)}>
      <DiskScanFeedback scan={scan} />
      <p className="muted">You can close this window while the scan continues. Folder matching and file measurement run separately after the scan.</p>
      {!scan.busy && <button onClick={scan.start}>
        {scan.requestError || scan.status?.state === 'failed' ? 'Try again' : 'Scan again'}
      </button>}
    </ActionDialog>}
  </>
}
