import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { DownloadClientInput, IndexerInput } from '../api'
import {
  addDownloadClient,
  addIndexer,
  deleteDownloadClient,
  deleteIndexer,
  getDownloadClients,
  getIndexers,
  testDownloadClient,
  testIndexer,
} from '../api'

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
  const [cliMsg, setCliMsg] = useState('')

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
    mutationFn: () => testDownloadClient(cli),
    onSuccess: () => setCliMsg('✓ client reachable'),
    onError: (e) => setCliMsg(`✕ ${(e as Error).message}`),
  })
  const saveCli = useMutation({
    mutationFn: () => addDownloadClient({ ...cli, enabled: true }),
    onSuccess: () => {
      setCli({ type: cli.type, name: '', url: '', username: '', password: '', category: 'monarr' })
      setCliMsg('')
      void qc.invalidateQueries({ queryKey: ['downloadclients'] })
    },
    onError: (e) => setCliMsg(`✕ ${(e as Error).message}`),
  })
  const delCli = useMutation({
    mutationFn: deleteDownloadClient,
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['downloadclients'] }),
  })

  return (
    <>
      <section className="panel">
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

      <section className="panel">
        <h2>Download clients</h2>
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
          <select value={cli.type} onChange={(e) => setCli({ ...cli, type: e.target.value as DownloadClientInput['type'] })}>
            <option value="qbittorrent">qBittorrent</option>
            <option value="transmission">Transmission</option>
            <option value="deluge">Deluge</option>
            <option value="sabnzbd">SABnzbd</option>
            <option value="nzbget">NZBGet</option>
          </select>
          <input placeholder="Name" value={cli.name} onChange={(e) => setCli({ ...cli, name: e.target.value })} />
          <input placeholder="URL (http://qbittorrent:8080)" value={cli.url} onChange={(e) => setCli({ ...cli, url: e.target.value })} />
          {cli.type === 'sabnzbd' ? (
            <input type="password" placeholder="API key" value={cli.password} onChange={(e) => setCli({ ...cli, password: e.target.value })} />
          ) : cli.type === 'deluge' ? (
            <input type="password" placeholder="Web password" value={cli.password} onChange={(e) => setCli({ ...cli, password: e.target.value })} />
          ) : (
            <>
              <input placeholder="Username" value={cli.username} onChange={(e) => setCli({ ...cli, username: e.target.value })} />
              <input type="password" placeholder="Password" value={cli.password} onChange={(e) => setCli({ ...cli, password: e.target.value })} />
            </>
          )}
          <input placeholder="Category" value={cli.category} onChange={(e) => setCli({ ...cli, category: e.target.value })} style={{ minWidth: 110 }} />
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
