import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { DownloadClientConfig, DownloadClientInput, IndexerInput } from '../api'

// Default ports per client type; prepopulates the port box.
const CLIENT_DEFAULT_PORTS: Record<DownloadClientInput['type'], string> = {
  qbittorrent: '8080',
  transmission: '9091',
  deluge: '8112',
  sabnzbd: '8080',
  nzbget: '6789',
  nzbd: '6789',
}
import {
  composeHostPort,
  addDownloadClient,
  addIndexer,
  deleteDownloadClient,
  deleteIndexer,
  getDownloadClients,
  getIndexers,
  testDownloadClient,
  testDownloadClientById,
  testIndexer,
  testIndexerById,
  updateDownloadClient,
} from '../api'

// RowTest is the per-row Test button for saved indexers/clients: runs the
// reachability test against the STORED credentials and reports the result.
//
// The failure message is rendered, not tucked into a title attribute. It used
// to be a hover tooltip on a single ✕ glyph, which meant the one piece of
// information the button exists to produce — *why* it failed — was invisible
// unless you knew to hover a 9-pixel target, and unreachable entirely on a
// touch screen.
function RowTest(props: { run: () => Promise<unknown> }) {
  const [state, setState] = useState<'idle' | 'busy' | 'ok' | 'error'>('idle')
  const [message, setMessage] = useState('')
  const test = async () => {
    setState('busy')
    setMessage('')
    try {
      await props.run()
      setState('ok')
    } catch (e) {
      setState('error')
      setMessage((e as Error).message || 'failed')
    }
  }
  return (
    <span className="row-test">
      <button onClick={() => void test()} disabled={state === 'busy'}>
        {state === 'busy' ? 'Testing…' : 'Test'}
      </button>{' '}
      {state === 'ok' && <span className="ok-text">✓ reachable</span>}
      {state === 'error' && <span className="error-text test-error">✕ {message}</span>}
    </span>
  )
}

// Indexer + download client management, embedded in the Settings page.
export function AcquisitionSettings() {
  const qc = useQueryClient()
  const indexers = useQuery({ queryKey: ['indexers'], queryFn: getIndexers })
  const clients = useQuery({ queryKey: ['downloadclients'], queryFn: getDownloadClients })

  const [idx, setIdx] = useState<IndexerInput>({ name: '', url: '', apiKey: '', protocol: 'torrent' })
  const [idxMsg, setIdxMsg] = useState('')
  const [cli, setCli] = useState<DownloadClientInput>({
    type: 'qbittorrent', name: '', url: '', username: '', password: '', category: 'monarr',
  })
  const [cliPort, setCliPort] = useState(CLIENT_DEFAULT_PORTS.qbittorrent)
  const [cliMsg, setCliMsg] = useState('')
  // Remote path mapping (optional): the client's completed folder as IT
  // reports it, and the path Monarr sees the same files at.
  const [mapRemote, setMapRemote] = useState('')
  const [mapLocal, setMapLocal] = useState('')
  const cliPayload = (): DownloadClientInput => ({
    ...cli,
    url: composeHostPort(cli.url, cliPort),
    pathMappings:
      mapRemote.trim() && mapLocal.trim()
        ? [{ remote: mapRemote.trim(), local: mapLocal.trim() }]
        : undefined,
  })

  const testIdx = useMutation({
    mutationFn: () => testIndexer(idx),
    onSuccess: () => setIdxMsg('✓ indexer reachable'),
    onError: (e) => setIdxMsg(`✕ ${(e as Error).message}`),
  })
  const saveIdx = useMutation({
    mutationFn: () => addIndexer({ ...idx, enabled: true }),
    onSuccess: () => {
      setIdx({ name: '', url: '', apiKey: '', protocol: idx.protocol })
      setIdxMsg('')
      void qc.invalidateQueries({ queryKey: ['indexers'] })
    },
    onError: (e) => setIdxMsg(`✕ ${(e as Error).message}`),
  })
  const delIdx = useMutation({
    mutationFn: deleteIndexer,
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['indexers'] }),
  })

  const testCli = useMutation({
    mutationFn: () => testDownloadClient(cliPayload()),
    onSuccess: () => setCliMsg('✓ client reachable'),
    onError: (e) => setCliMsg(`✕ ${(e as Error).message}`),
  })
  const saveCli = useMutation({
    mutationFn: () => addDownloadClient({ ...cliPayload(), enabled: true }),
    onSuccess: () => {
      setCli({ type: cli.type, name: '', url: '', username: '', password: '', category: 'monarr' })
      setCliPort(CLIENT_DEFAULT_PORTS[cli.type])
      setMapRemote('')
      setMapLocal('')
      setCliMsg('')
      void qc.invalidateQueries({ queryKey: ['downloadclients'] })
    },
    onError: (e) => setCliMsg(`✕ ${(e as Error).message}`),
  })
  const delCli = useMutation({
    mutationFn: deleteDownloadClient,
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['downloadclients'] }),
  })
  // Toggle manual-approval on a saved client. The masked password round-trips
  // and is preserved server-side, so this never blanks credentials.
  const toggleApproval = useMutation({
    mutationFn: (c: DownloadClientConfig) =>
      updateDownloadClient(c.id, {
        type: c.type, name: c.name, url: c.url,
        username: c.username, password: c.password,
        category: c.category, enabled: c.enabled,
        manualApproval: !c.manualApproval, pathMappings: c.pathMappings,
      }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['downloadclients'] }),
  })

  return (
    <>
      <section className="panel" id="indexers">
        <h2>Indexers</h2>
        <p className="muted">
          Newznab/Torznab endpoints — point these at Prowlarr or Jackett proxies, or directly at
          an indexer's API. Torrent indexers feed qBittorrent; usenet feeds SABnzbd.
        </p>
        <table>
          <tbody>
            {indexers.data?.map((i) => (
              <tr key={i.id}>
                <td>{i.name}</td>
                <td className="mono muted">{i.url}</td>
                <td>
                  <span className="pill pill-neutral">{i.protocol}</span>
                </td>
                <td>
                  <RowTest run={() => testIndexerById(i.id)} />
                </td>
                <td>
                  <button onClick={() => delIdx.mutate(i.id)}>Remove</button>
                </td>
              </tr>
            ))}
            {indexers.data?.length === 0 && (
              <tr>
                <td className="muted">No indexers yet.</td>
              </tr>
            )}
          </tbody>
        </table>
        <div className="form-row">
          <input placeholder="Name" value={idx.name} onChange={(e) => setIdx({ ...idx, name: e.target.value })} />
          <input placeholder="URL (http://prowlarr:9696/1)" value={idx.url} onChange={(e) => setIdx({ ...idx, url: e.target.value })} />
          <input placeholder="API key" value={idx.apiKey} onChange={(e) => setIdx({ ...idx, apiKey: e.target.value })} />
          <select value={idx.protocol} onChange={(e) => setIdx({ ...idx, protocol: e.target.value as 'torrent' | 'usenet' })}>
            <option value="torrent">torrent</option>
            <option value="usenet">usenet</option>
          </select>
          <button onClick={() => testIdx.mutate()} disabled={testIdx.isPending || !idx.url}>
            Test
          </button>
          <button className="btn-accent" onClick={() => saveIdx.mutate()} disabled={saveIdx.isPending || !idx.name || !idx.url}>
            Add indexer
          </button>
        </div>
        {idxMsg && <p className={idxMsg.startsWith('✓') ? 'ok-text' : 'error-text'}>{idxMsg}</p>}
      </section>

      <section className="panel" id="downloadclients">
        <h2>Download clients</h2>
        <p className="muted">
          Downloads land wherever the client itself is configured to put finished files
          (NZBGet's DestDir / category folder, qBittorrent's save path, …) — Monarr then
          imports from the path the client reports. That path must be visible to Monarr; if
          the client runs on another host or container and reports a different path than
          Monarr mounts, set a remote path mapping below.
        </p>
        <table>
          <tbody>
            {clients.data?.map((c) => (
              <tr key={c.id}>
                <td>{c.name}</td>
                <td>
                  <span className="pill pill-neutral">{c.type}</span>
                </td>
                <td className="mono muted">{c.url}</td>
                <td className="muted">{c.category}</td>
                <td className="mono muted">
                  {c.pathMappings?.length
                    ? `${c.pathMappings[0].remote} → ${c.pathMappings[0].local}`
                    : ''}
                </td>
                <td>
                  <label
                    className="approval-toggle"
                    title="Hold this client's completed downloads for manual approval before import"
                  >
                    <input
                      type="checkbox"
                      checked={!!c.manualApproval}
                      onChange={() => toggleApproval.mutate(c)}
                      disabled={toggleApproval.isPending}
                    />{' '}
                    Approve imports
                  </label>
                </td>
                <td>
                  <RowTest run={() => testDownloadClientById(c.id)} />
                </td>
                <td>
                  <button onClick={() => delCli.mutate(c.id)}>Remove</button>
                </td>
              </tr>
            ))}
            {clients.data?.length === 0 && (
              <tr>
                <td className="muted">No download clients yet.</td>
              </tr>
            )}
          </tbody>
        </table>
        <div className="form-row">
          <select
            value={cli.type}
            onChange={(e) => {
              const type = e.target.value as DownloadClientInput['type']
              setCli({ ...cli, type })
              setCliPort(CLIENT_DEFAULT_PORTS[type])
            }}
          >
            <option value="qbittorrent">qBittorrent</option>
            <option value="transmission">Transmission</option>
            <option value="deluge">Deluge</option>
            <option value="sabnzbd">SABnzbd</option>
            <option value="nzbget">NZBGet</option>
            <option value="nzbd">nzbd (native)</option>
          </select>
          <input placeholder="Name" value={cli.name} onChange={(e) => setCli({ ...cli, name: e.target.value })} />
          <input
            placeholder="Host (192.168.1.10)"
            value={cli.url}
            onChange={(e) => setCli({ ...cli, url: e.target.value })}
          />
          <input
            placeholder="Port"
            aria-label="Port"
            style={{ width: 80 }}
            value={cliPort}
            onChange={(e) => setCliPort(e.target.value.replace(/[^0-9]/g, ''))}
          />
          {cli.type === 'sabnzbd' ? (
            <input type="password" placeholder="API key" value={cli.password} onChange={(e) => setCli({ ...cli, password: e.target.value })} />
          ) : cli.type === 'nzbd' ? (
            // nzbd takes either a token or a username/password on the same
            // header. Leave the username blank and the password field is
            // read as a token, which is the credential its docs hand out.
            <>
              <input placeholder="Username (blank if using a token)" value={cli.username} onChange={(e) => setCli({ ...cli, username: e.target.value })} />
              <input type="password" placeholder="Token, or password" value={cli.password} onChange={(e) => setCli({ ...cli, password: e.target.value })} />
            </>
          ) : cli.type === 'deluge' ? (
            <input type="password" placeholder="Web password" value={cli.password} onChange={(e) => setCli({ ...cli, password: e.target.value })} />
          ) : (
            <>
              <input placeholder="Username (blank if no auth)" value={cli.username} onChange={(e) => setCli({ ...cli, username: e.target.value })} />
              <input type="password" placeholder="Password (blank if no auth)" value={cli.password} onChange={(e) => setCli({ ...cli, password: e.target.value })} />
            </>
          )}
          <input placeholder="Category" value={cli.category} onChange={(e) => setCli({ ...cli, category: e.target.value })} style={{ minWidth: 110 }} />
        </div>
        <div className="form-row">
          <input
            placeholder="Remote path the client reports (optional, e.g. /data/completed)"
            aria-label="Remote path"
            value={mapRemote}
            onChange={(e) => setMapRemote(e.target.value)}
            style={{ minWidth: 260 }}
          />
          <span className="muted">→</span>
          <input
            placeholder="Same folder as Monarr sees it (e.g. /pool/downloads)"
            aria-label="Local path"
            value={mapLocal}
            onChange={(e) => setMapLocal(e.target.value)}
            style={{ minWidth: 260 }}
          />
          <label className="approval-toggle" title="Hold completed downloads for manual approval before import">
            <input
              type="checkbox"
              checked={!!cli.manualApproval}
              onChange={(e) => setCli({ ...cli, manualApproval: e.target.checked })}
            />{' '}
            Require approval
          </label>
          <button onClick={() => testCli.mutate()} disabled={testCli.isPending || !cli.url}>
            Test
          </button>
          <button className="btn-accent" onClick={() => saveCli.mutate()} disabled={saveCli.isPending || !cli.name || !cli.url}>
            Add client
          </button>
        </div>
        {cliMsg && <p className={cliMsg.startsWith('✓') ? 'ok-text' : 'error-text'}>{cliMsg}</p>}
      </section>
    </>
  )
}
