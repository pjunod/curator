import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { getLibrary } from './api'

// One destination the sidebar search can jump to. Settings sections carry a
// hash that matches the section's id attribute for scroll-into-view.
interface Destination {
  label: string
  sub: string
  to: string
  hash?: string
  search?: Record<string, unknown>
  keywords: string
}

const PAGES: Destination[] = [
  { label: 'Library', sub: 'page', to: '/', keywords: 'library home posters movies series books' },
  { label: 'Add media', sub: 'page', to: '/add', keywords: 'add new search tmdb movie series book' },
  { label: 'Discover', sub: 'page', to: '/discover', keywords: 'discover browse trending popular top rated new releases upcoming anticipated box office trakt what to watch' },
  { label: 'Wanted', sub: 'page', to: '/wanted', keywords: 'wanted missing cutoff upgrade backlog' },
  { label: 'Calendar', sub: 'page', to: '/calendar', keywords: 'calendar schedule air dates releases' },
  { label: 'Activity', sub: 'page', to: '/activity', keywords: 'activity queue downloads history blocklist' },
  { label: 'Dashboard', sub: 'page', to: '/dashboard', keywords: 'dashboard status overview stats' },
  { label: 'System', sub: 'page', to: '/system', keywords: 'system tasks backups logs health version' },
  { label: 'Settings', sub: 'page', to: '/settings', keywords: 'settings configuration' },
  { label: 'Access', sub: 'page', to: '/access', keywords: 'access users login security api key mobile pairing qr code' },
  { label: 'User login', sub: 'access', to: '/access', hash: 'user-login', keywords: 'users login username password authentication security' },
  { label: 'Mobile pairing', sub: 'access', to: '/access', hash: 'mobile-pairing', keywords: 'mobile phone tablet app qr code link pair connect' },
  { label: 'API access', sub: 'access', to: '/access', hash: 'api-access', keywords: 'api key integration sonarr radarr reveal credential' },
  { label: 'Metadata provider', sub: 'setting', to: '/settings', hash: 'metadata', keywords: 'tmdb api key metadata provider' },
  { label: 'Root folders', sub: 'setting', to: '/settings', hash: 'rootfolders', keywords: 'root folders storage library paths disk' },
  { label: 'Disk scan', sub: 'setting', to: '/settings', hash: 'scan', keywords: 'scan disk rescan reconcile import existing' },
  { label: 'Indexers', sub: 'setting', to: '/settings', hash: 'indexers', keywords: 'indexers usenet torrent newznab torznab drunkenslug prowlarr jackett' },
  { label: 'Download clients', sub: 'setting', to: '/settings', hash: 'downloadclients', keywords: 'download clients nzbget sabnzbd qbittorrent transmission deluge path mapping' },
  { label: 'Custom formats', sub: 'setting', to: '/settings', hash: 'customformats', keywords: 'custom formats scoring preferred words' },
  { label: 'Import lists', sub: 'setting', to: '/settings', hash: 'importlists', keywords: 'import lists trakt tmdb popular discover' },
  { label: 'Notifications', sub: 'setting', to: '/settings', hash: 'notifications', keywords: 'notifications webhook discord plex jellyfin plurx' },
]

// GlobalSearch is the sidebar's find-anything box: library items by title,
// pages and settings sections by name or keyword.
export function GlobalSearch() {
  const [q, setQ] = useState('')
  const [open, setOpen] = useState(false)
  const navigate = useNavigate()
  const boxRef = useRef<HTMLDivElement>(null)

  const library = useQuery({
    queryKey: ['library', 'all'],
    queryFn: () => getLibrary(),
    enabled: q.trim().length >= 2,
    staleTime: 30_000,
  })

  useEffect(() => {
    const onClick = (e: MouseEvent) => {
      if (boxRef.current && !boxRef.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onClick)
    return () => document.removeEventListener('mousedown', onClick)
  }, [])

  const needle = q.trim().toLowerCase()
  const pageHits = needle
    ? PAGES.filter(
        (p) => p.label.toLowerCase().includes(needle) || p.keywords.includes(needle),
      ).slice(0, 5)
    : []
  const mediaHits =
    needle.length >= 2
      ? (library.data ?? [])
          .filter(
            (m) =>
              m.title.toLowerCase().includes(needle) ||
              (m.author && m.author.toLowerCase().includes(needle)),
          )
          .slice(0, 6)
      : []

  const close = () => {
    setQ('')
    setOpen(false)
  }
  const goPage = (d: Destination) => {
    close()
    void navigate({ to: d.to, search: (d.search ?? {}) as never })
    if (d.hash) {
      // Give the settings page a beat to render, then bring the section in.
      setTimeout(() => document.getElementById(d.hash!)?.scrollIntoView({ behavior: 'smooth' }), 150)
    }
  }
  const goItem = (id: number) => {
    close()
    void navigate({ to: '/library/$id', params: { id: String(id) } })
  }
  const goAdd = () => {
    const query = q.trim()
    close()
    void navigate({ to: '/add', search: { q: query } as never })
  }
  const first = () => {
    if (mediaHits.length > 0) goItem(mediaHits[0].id)
    else if (pageHits.length > 0) goPage(pageHits[0])
    else if (needle) goAdd()
  }

  return (
    <div className="global-search" ref={boxRef}>
      {open && needle && (
        <div className="search-results" role="listbox">
          {mediaHits.map((m) => (
            <button key={`m${m.id}`} className="search-hit" onClick={() => goItem(m.id)}>
              <span className="search-hit-label">{m.title}</span>
              <span className="search-hit-sub">
                {m.kind === 'book' && m.author ? m.author : m.year || m.kind}
              </span>
            </button>
          ))}
          {pageHits.map((p) => (
            <button key={p.label} className="search-hit" onClick={() => goPage(p)}>
              <span className="search-hit-label">{p.label}</span>
              <span className="search-hit-sub">{p.sub}</span>
            </button>
          ))}
          <button className="search-hit" onClick={goAdd} title={`Search TMDB / Open Library for “${q.trim()}”`}>
            <span className="search-hit-label">Search providers for “{q.trim()}”…</span>
          </button>
        </div>
      )}
      <input
        type="search"
        placeholder="Search media & settings…"
        aria-label="Search media and settings"
        value={q}
        onChange={(e) => {
          setQ(e.target.value)
          setOpen(true)
        }}
        onFocus={() => setOpen(true)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') first()
          if (e.key === 'Escape') close()
        }}
      />
    </div>
  )
}
