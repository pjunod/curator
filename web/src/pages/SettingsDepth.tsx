import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import QRCode from 'react-qr-code'
import type { ImportList } from '../api'
import {
  addCustomFormat, addImportList, deleteCustomFormat, deleteImportList,
  getCustomFormats, getImportLists, getProfiles, getRootFolders, getSettings,
  updateSettings,
} from '../api'
import { buildPairingCode } from '../pairing'

// CustomFormatSettings manages regex scoring rules (Phase 5).
export function CustomFormatSettings() {
  const qc = useQueryClient()
  const formats = useQuery({ queryKey: ['customformats'], queryFn: getCustomFormats })
  const [name, setName] = useState('')
  const [pattern, setPattern] = useState('')
  const [score, setScore] = useState('10')

  const add = useMutation({
    mutationFn: () => addCustomFormat({ name, pattern, score: Number(score) || 0 }),
    onSuccess: () => {
      setName('')
      setPattern('')
      void qc.invalidateQueries({ queryKey: ['customformats'] })
    },
  })
  const del = useMutation({
    mutationFn: deleteCustomFormat,
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['customformats'] }),
  })

  return (
    <section className="panel" id="customformats">
      <h2>Custom formats</h2>
      <p className="muted">
        Regex rules scored against release titles; higher totals win ties between
        equal-quality releases (negative scores bury releases).
      </p>
      {(formats.data?.length ?? 0) > 0 && (
        <table>
          <tbody>
            {formats.data!.map((f) => (
              <tr key={f.id}>
                <td>{f.name}</td>
                <td className="mono">{f.pattern}</td>
                <td className="muted">{f.score}</td>
                <td>
                  <button onClick={() => del.mutate(f.id)}>Remove</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div className="add-controls" style={{ marginTop: 12 }}>
        <input placeholder="Name" value={name} onChange={(e) => setName(e.target.value)} />
        <input placeholder="Pattern (regex)" value={pattern} onChange={(e) => setPattern(e.target.value)} />
        <input placeholder="Score" style={{ width: 70 }} value={score} onChange={(e) => setScore(e.target.value)} />
        <button className="btn-accent" disabled={!name || !pattern || add.isPending} onClick={() => add.mutate()}>
          Add
        </button>
      </div>
      {add.isError && <div className="banner warning">{String((add.error as Error).message)}</div>}
    </section>
  )
}

// ImportListSettings manages auto-add list sources (Phase 5).
export function ImportListSettings() {
  const qc = useQueryClient()
  const lists = useQuery({ queryKey: ['importlists'], queryFn: getImportLists })
  const roots = useQuery({ queryKey: ['rootfolders'], queryFn: getRootFolders })
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: getProfiles })

  const [type, setType] = useState<ImportList['type']>('tmdb-popular')
  const [name, setName] = useState('')
  const [config, setConfig] = useState<Record<string, string>>({})
  const [rootId, setRootId] = useState<number | undefined>()
  const [profileId, setProfileId] = useState(1)

  const add = useMutation({
    mutationFn: () =>
      addImportList({
        name, type, config, kind: 'movie',
        rootFolderId: rootId, qualityProfileId: profileId, monitored: true, enabled: true,
      }),
    onSuccess: () => {
      setName('')
      setConfig({})
      void qc.invalidateQueries({ queryKey: ['importlists'] })
    },
  })
  const del = useMutation({
    mutationFn: deleteImportList,
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['importlists'] }),
  })

  return (
    <section className="panel" id="importlists">
      <h2>Import lists</h2>
      <p className="muted">
        External lists synced every 12 hours; new entries are added to the library
        with the list's folder, profile, and monitoring policy.
      </p>
      {(lists.data?.length ?? 0) > 0 && (
        <table>
          <tbody>
            {lists.data!.map((l) => (
              <tr key={l.id}>
                <td>{l.name}</td>
                <td>
                  <span className="pill pill-neutral">{l.type}</span>
                </td>
                <td>
                  <button onClick={() => del.mutate(l.id)}>Remove</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div className="add-controls" style={{ marginTop: 12 }}>
        <select value={type} onChange={(e) => setType(e.target.value as ImportList['type'])}>
          <option value="tmdb-popular">TMDB Popular</option>
          <option value="tmdb-top">TMDB Top Rated</option>
          <option value="trakt-list">Trakt list</option>
        </select>
        <input placeholder="Name" value={name} onChange={(e) => setName(e.target.value)} />
        {type === 'trakt-list' && (
          <>
            <input placeholder="Trakt user" value={config.user ?? ''}
              onChange={(e) => setConfig({ ...config, user: e.target.value })} />
            <input placeholder="List slug" value={config.slug ?? ''}
              onChange={(e) => setConfig({ ...config, slug: e.target.value })} />
            <input placeholder="Trakt client id" value={config.clientId ?? ''}
              onChange={(e) => setConfig({ ...config, clientId: e.target.value })} />
          </>
        )}
        <select value={rootId ?? ''} onChange={(e) => setRootId(e.target.value ? Number(e.target.value) : undefined)}>
          <option value="">(no folder)</option>
          {roots.data?.map((rf) => (
            <option key={rf.id} value={rf.id}>{rf.path}</option>
          ))}
        </select>
        <select value={profileId} onChange={(e) => setProfileId(Number(e.target.value))}>
          {profiles.data?.map((p) => (
            <option key={p.id} value={p.id}>{p.name}</option>
          ))}
        </select>
        <button className="btn-accent" disabled={!name || add.isPending} onClick={() => add.mutate()}>
          Add
        </button>
      </div>
      {add.isError && <div className="banner warning">{String((add.error as Error).message)}</div>}
    </section>
  )
}

// MobilePairingSettings deliberately stands on its own on the Access page.
// Pairing used to be nested behind API-key reveal in Security,
// which made a shipped feature indistinguishable from a missing one.
export function MobilePairingSettings() {
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const [pairingVisible, setPairingVisible] = useState(false)
  const [pairingAddress, setPairingAddress] = useState(() =>
    typeof window === 'undefined' ? '' : window.location.origin,
  )
  const apiKey = settings.data?.apiKey ?? ''

  return (
    <section className="panel mobile-pairing-panel" id="mobile-pairing">
      <h2>Mobile pairing</h2>
      <p className="muted">
        Enter the address this phone can reach, then show and scan the code from the
        Curator app. The QR contains the API key, so show it only to a device you trust.
      </p>
      <div className="add-controls">
        <input
          aria-label="Mobile pairing server address"
          value={pairingAddress}
          placeholder="http://192.168.1.20:7676"
          onChange={(event) => setPairingAddress(event.target.value)}
        />
        <button
          className="btn-accent"
          disabled={!pairingAddress.trim() || !apiKey}
          onClick={() => setPairingVisible((value) => !value)}
        >
          {pairingVisible ? 'Hide pairing QR' : 'Show pairing QR'}
        </button>
      </div>
      {settings.isPending && <p className="muted">Loading the pairing key…</p>}
      {settings.isError && (
        <div className="banner warning">The pairing key could not be loaded. Reload Access and try again.</div>
      )}
      {pairingVisible && apiKey && (
        <div className="pairing-code">
          <QRCode
            aria-label="Curator mobile pairing QR code"
            bgColor="#ffffff"
            fgColor="#111111"
            level="M"
            size={220}
            value={buildPairingCode(pairingAddress, apiKey)}
          />
          <span className="muted">Curator app → Scan pairing QR</span>
        </div>
      )}
    </section>
  )
}

// UserLoginSettings manages browser-session authentication. It lives on the
// Access page because login, mobile pairing, and API credentials are three
// ways into the same server rather than general application settings.
export function UserLoginSettings() {
  const qc = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')

  const save = useMutation({
    mutationFn: (enable: boolean) =>
      updateSettings({
        authRequired: enable,
        ...(username ? { authUsername: username } : {}),
        ...(password ? { authPassword: password } : {}),
      }),
    onSuccess: () => {
      setPassword('')
      void qc.invalidateQueries({ queryKey: ['settings'] })
    },
  })

  const authOn = settings.data?.authRequired ?? false
  return (
    <section className="panel" id="user-login">
      <h2>User login</h2>
      <p className="muted">
        Enabling authentication locks the Curator API and web interface behind a login.
        The API key remains a separate bypass for trusted apps and integrations.
      </p>
      <div className="detail-facts">
        <span className={authOn ? 'pill pill-ok' : 'pill pill-neutral'}>
          auth {authOn ? 'enabled' : 'disabled'}
        </span>
      </div>
      <div className="add-controls" style={{ marginTop: 12 }}>
        <input placeholder="Username" autoComplete="off" value={username}
          onChange={(e) => setUsername(e.target.value)} />
        <input placeholder="Password" type="password" autoComplete="new-password" value={password}
          onChange={(e) => setPassword(e.target.value)} />
        {!authOn ? (
          <button className="btn-accent" disabled={save.isPending} onClick={() => save.mutate(true)}>
            Save &amp; enable auth
          </button>
        ) : (
          <button className="btn-danger" disabled={save.isPending} onClick={() => save.mutate(false)}>
            Disable auth
          </button>
        )}
      </div>
      {save.isError && <div className="banner warning">{String((save.error as Error).message)}</div>}
    </section>
  )
}

// APIKeySettings exposes the integration credential only after an explicit
// action. Mobile pairing uses the same key without requiring its raw value to
// be placed on screen.
export function APIKeySettings() {
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const [revealed, setRevealed] = useState(false)

  return (
    <section className="panel" id="api-access">
      <h2>API access</h2>
      <p className="muted">
        Trusted apps authenticate with X-Api-Key, including the /sonarr and /radarr
        compatibility endpoints. Keep this credential private.
      </p>
      <div className="detail-facts">
        <span>API key:</span>
        {revealed ? (
          <span className="mono">{settings.data?.apiKey ?? '—'}</span>
        ) : (
          <button onClick={() => setRevealed(true)}>Reveal</button>
        )}
      </div>
    </section>
  )
}
