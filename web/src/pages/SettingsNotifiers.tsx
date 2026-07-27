import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { NotifierInput, NotifierType } from '../api'
import { addNotifier, deleteNotifier, getNotifiers, testNotifier } from '../api'

const TYPE_FIELDS: Record<NotifierType, { key: string; label: string }[]> = {
  webhook: [{ key: 'url', label: 'Webhook URL' }],
  discord: [{ key: 'url', label: 'Discord webhook URL' }],
  plex: [
    { key: 'url', label: 'Plex server URL' },
    { key: 'token', label: 'X-Plex-Token' },
  ],
  jellyfin: [
    { key: 'url', label: 'Jellyfin server URL' },
    { key: 'apiKey', label: 'API key' },
  ],
  plurx: [
    { key: 'url', label: 'plurx server URL' },
    { key: 'apiKey', label: 'Scoped key (plx_…, scan:trigger)' },
  ],
}

// The types that act on a media server rather than talk to a person. They
// fire on imports only, so the per-event checkboxes do not apply to them.
const MEDIA_SERVER: NotifierType[] = ['plex', 'jellyfin', 'plurx']

// NotifierSettings manages notification targets (Phase 3): webhooks,
// Discord, the Plex/Jellyfin library-refresh pokes, and plurx — which is
// not a poke: it is told exactly which paths landed and what they are.
export function NotifierSettings() {
  const qc = useQueryClient()
  const notifiers = useQuery({ queryKey: ['notifiers'], queryFn: getNotifiers })

  const [type, setType] = useState<NotifierType>('webhook')
  const [name, setName] = useState('')
  const [settings, setSettings] = useState<Record<string, string>>({})
  const [testResult, setTestResult] = useState('')

  const input = (): NotifierInput => ({ type, name, settings })

  const add = useMutation({
    mutationFn: () => addNotifier(input()),
    onSuccess: () => {
      setName('')
      setSettings({})
      setTestResult('')
      void qc.invalidateQueries({ queryKey: ['notifiers'] })
    },
  })
  const test = useMutation({
    mutationFn: () => testNotifier(input()),
    onSuccess: () => setTestResult('✓ delivered'),
    onError: (e) => setTestResult(`✗ ${(e as Error).message}`),
  })
  const del = useMutation({
    mutationFn: deleteNotifier,
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['notifiers'] }),
  })

  return (
    <section className="panel" id="notifications">
      <h2>Notifications</h2>
      <p className="muted">
        Webhook and Discord targets get grab/import/failure events. Plex and Jellyfin are
        poked to rescan their libraries after every import. plurx is told the exact paths
        that landed and the TMDB/IMDb ids Monarr already knows, so it indexes one folder
        instead of sweeping the library — and never has to guess the title from a filename.
      </p>

      {(notifiers.data?.length ?? 0) > 0 && (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Type</th>
              <th>Events</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {notifiers.data!.map((n) => (
              <tr key={n.id}>
                <td>{n.name}</td>
                <td>
                  <span className="pill pill-neutral">{n.type}</span>
                </td>
                <td className="muted">
                  {MEDIA_SERVER.includes(n.type)
                    ? n.type === 'plurx'
                      ? 'on import (targeted scan)'
                      : 'on import (library refresh)'
                    : [n.onGrab && 'grab', n.onImport && 'import', n.onFailed && 'failed', n.onHealth && 'health']
                        .filter(Boolean)
                        .join(', ')}
                </td>
                <td>
                  <button onClick={() => del.mutate(n.id)}>Remove</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <div className="add-controls" style={{ marginTop: 12 }}>
        <select
          value={type}
          onChange={(e) => {
            setType(e.target.value as NotifierType)
            setSettings({})
            setTestResult('')
          }}
        >
          <option value="webhook">Webhook</option>
          <option value="discord">Discord</option>
          <option value="plex">Plex refresh</option>
          <option value="jellyfin">Jellyfin refresh</option>
          <option value="plurx">plurx targeted scan</option>
        </select>
        <input
          placeholder="Name"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        {TYPE_FIELDS[type].map((f) => (
          <input
            key={f.key}
            placeholder={f.label}
            value={settings[f.key] ?? ''}
            onChange={(e) => setSettings({ ...settings, [f.key]: e.target.value })}
          />
        ))}
        <button onClick={() => test.mutate()} disabled={test.isPending}>
          Test
        </button>
        <button
          className="btn-accent"
          onClick={() => add.mutate()}
          disabled={add.isPending || !name}
        >
          Add
        </button>
        {testResult && <span className="muted">{testResult}</span>}
      </div>
      {add.isError && <div className="banner warning">{String((add.error as Error).message)}</div>}
    </section>
  )
}
