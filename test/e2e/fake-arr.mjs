// A fake Torznab indexer + fake qBittorrent for end-to-end tests, on one
// port. The qbit side "completes" a torrent instantly by materializing a
// payload directory with fake video files, so the full search → grab →
// import loop runs without any real services.
import { createServer } from 'node:http'
import { mkdirSync, writeFileSync, mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const PORT = Number(process.env.FAKE_ARR_PORT ?? 7799)
const dlRoot = mkdtempSync(join(tmpdir(), 'monarr-e2e-qbit-'))

const releasesXML = (items) => `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>
${items.join('\n')}
</channel></rss>`

const item = (title, slug, size, seeders) => `<item>
  <title>${title}</title>
  <guid>http://fake/details/${slug}</guid>
  <link>http://127.0.0.1:${PORT}/dl/${slug}</link>
  <pubDate>Fri, 17 Jul 2026 10:00:00 +0000</pubDate>
  <enclosure url="http://127.0.0.1:${PORT}/dl/${slug}" length="${size}" type="application/x-bittorrent"/>
  <torznab:attr name="seeders" value="${seeders}"/>
</item>`

// slug → payload spec
const payloads = {
  movie601: {
    name: 'The.Test.Movie.2024.1080p.WEB-DL.x264-E2E',
    files: ['The.Test.Movie.2024.1080p.WEB-DL.x264-E2E.mkv'],
  },
  movie601cam: {
    name: 'The.Test.Movie.2024.HDCAM.x264-JUNK',
    files: ['The.Test.Movie.2024.HDCAM.x264-JUNK.mkv'],
  },
  pack700: {
    name: 'The.Test.Show.S01.1080p.WEB-DL-E2E',
    files: ['The.Test.Show.S01E01.1080p.WEB-DL.mkv', 'The.Test.Show.S01E02.1080p.WEB-DL.mkv'],
  },
  book900: {
    name: 'Test Author - The Test Book (2024) EPUB',
    files: ['Test Author - The Test Book.epub'],
  },
  book900m4b: {
    name: 'The Test Book by Test Author M4B',
    files: ['The Test Book.m4b'],
  },
}

const torrents = [] // {hash, name, content_path}

createServer((req, res) => {
  const url = new URL(req.url, 'http://x')
  const q = url.searchParams

  // ---- torznab ----
  if (url.pathname === '/api') {
    res.setHeader('content-type', 'application/xml')
    if (q.get('t') === 'caps') {
      res.end('<caps><server title="fake"/></caps>')
      return
    }
    if (q.get('t') === 'tvsearch' && !q.get('ep')) {
      res.end(releasesXML([
        item('The.Test.Show.S01.1080p.WEB-DL-E2E', 'pack700', 4000000, 33),
      ]))
      return
    }
    if (q.get('t') === 'tvsearch') {
      res.end(releasesXML([
        item('The.Test.Show.S01E01.1080p.WEB-DL-E2E', 'pack700', 1000000, 12),
      ]))
      return
    }
    // book search: t=search with the Newznab book categories (7000s/3030).
    // One EPUB (accepted by the Ebook profile) and one M4B audiobook
    // (rejected: not in the Ebook profile's allowed set).
    if ((q.get('cat') ?? '').includes('7000')) {
      res.end(releasesXML([
        item('Test Author - The Test Book (2024) EPUB', 'book900', 800000, 21),
        item('The Test Book by Test Author M4B', 'book900m4b', 300000000, 9),
      ]))
      return
    }
    // movie search: one good candidate, one CAM (rejected by profile rank order)
    res.end(releasesXML([
      item('The.Test.Movie.2024.1080p.WEB-DL.x264-E2E', 'movie601', 2000000, 50),
      item('The.Test.Movie.2024.HDCAM.x264-JUNK', 'movie601cam', 900000, 2),
    ]))
    return
  }

  // ---- qbittorrent ----
  if (url.pathname === '/api/v2/auth/login') {
    res.setHeader('set-cookie', 'SID=e2e; Path=/')
    res.end('Ok.')
    return
  }
  if (url.pathname === '/api/v2/app/version') {
    res.end('v5.0.0')
    return
  }
  if (url.pathname === '/api/v2/torrents/add') {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      const params = new URLSearchParams(body)
      const dlURL = params.get('urls') ?? ''
      const slug = dlURL.split('/').pop()
      const spec = payloads[slug]
      if (!spec) {
        res.end('Fails.')
        return
      }
      const dir = join(dlRoot, spec.name)
      mkdirSync(dir, { recursive: true })
      for (const f of spec.files) writeFileSync(join(dir, f), `fake video ${f}`)
      torrents.push({ hash: `hash-${slug}-${torrents.length}`, name: spec.name, content_path: dir })
      res.end('Ok.')
    })
    return
  }
  if (url.pathname === '/api/v2/torrents/info') {
    res.setHeader('content-type', 'application/json')
    res.end(JSON.stringify(torrents.map((t) => ({
      hash: t.hash, name: t.name, progress: 1, state: 'stalledUP',
      save_path: dlRoot, content_path: t.content_path,
    }))))
    return
  }
  res.statusCode = 404
  res.end('not found')
}).listen(PORT, '127.0.0.1', () => console.log(`fake arr services on :${PORT}`))
