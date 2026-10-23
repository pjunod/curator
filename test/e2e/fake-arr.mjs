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

// Sizes are realistic on purpose. monarr declines a release whose advertised
// size cannot hold what its name claims (a 1080p feature at 2 MB is 2.6 kbps),
// so the toy sizes these fixtures used to carry now correctly fail to grab.
// The numbers below are what the named quality actually weighs: roughly
// 5 Mbps for a 1080p WEB-DL, over the runtime fake-tmdb reports.
const item = (title, slug, size, seeders) => `<item>
  <title>${title}</title>
  <guid>http://fake/details/${slug}</guid>
  <link>http://127.0.0.1:${PORT}/dl/${slug}</link>
  <pubDate>Fri, 17 Jul 2026 10:00:00 +0000</pubDate>
  <enclosure url="http://127.0.0.1:${PORT}/dl/${slug}" length="${size}" type="application/x-bittorrent"/>
  <torznab:attr name="seeders" value="${seeders}"/>
</item>`

const automaticSeasonPack = item('The.Test.Show.S01.1080p.WEB-DL-AUTO', 'pack700auto', 2_200_000_000, 100)

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
  // RSS uses different native torrent hashes from the explicit manual-copy
  // fixtures below. qBittorrent correctly retains the tag of an already
  // present hash, so reusing one release here cannot prove a distinct
  // automatic primary acquisition.
  movie601auto: {
    name: 'The.Test.Movie.2024.1080p.WEB-DL.x264-AUTO',
    files: ['The.Test.Movie.2024.1080p.WEB-DL.x264-AUTO.mkv'],
  },
  movie601autoregrab: {
    name: 'The.Test.Movie.2024.1080p.WEB-DL.x264-AUTO-REGRAB',
    files: ['The.Test.Movie.2024.1080p.WEB-DL.x264-AUTO-REGRAB.mkv'],
  },
  pack700auto: {
    name: 'The.Test.Show.S01.1080p.WEB-DL-AUTO',
    files: ['The.Test.Show.S01E01.1080p.WEB-DL-AUTO.mkv', 'The.Test.Show.S01E02.1080p.WEB-DL-AUTO.mkv'],
  },
  book900: {
    name: 'Test Author - The Test Book (2024) EPUB',
    files: ['Test Author - The Test Book.epub'],
  },
  book900auto: {
    name: 'Test Author - The Test Book (2024) EPUB AUTO',
    files: ['Test Author - The Test Book.epub'],
  },
  book900m4b: {
    name: 'The Test Book by Test Author M4B',
    files: ['The Test Book.m4b'],
  },
  book900m4bauto: {
    name: 'The Test Book by Test Author M4B AUTO',
    files: ['The Test Book.m4b'],
  },
  book901m4b: {
    name: 'Audio Author - The Test Audiobook M4B',
    files: ['01 - Opening.m4b', '02 - Middle.m4b', '03 - Ending.m4b'],
  },
  book901m4bauto: {
    name: 'Audio Author - The Test Audiobook M4B AUTO',
    files: ['01 - Opening.m4b', '02 - Middle.m4b', '03 - Ending.m4b'],
  },
}

const torrents = [] // {hash, name, content_path}
const sinkLog = [] // webhook notifications received

createServer((req, res) => {
  const url = new URL(req.url, 'http://x')
  const q = url.searchParams

  // ---- torznab ----
  if (url.pathname === '/api') {
    res.setHeader('content-type', 'application/xml')
    if (q.get('t') === 'caps') {
      // Search planning reads these capabilities (internal/app/acquisition/
      // searchplan.go). Without them every query is a generic probe, and a
      // generic probe here answers with the movie catalogue — so a season
      // search never saw the pack and the series chain failed from there.
      res.end(
        '<caps><server title="fake"/><searching>' +
          '<search available="yes" supportedParams="q"/>' +
          '<tv-search available="yes" supportedParams="q,season,ep,tvdbid"/>' +
          '<movie-search available="yes" supportedParams="q,imdbid,tmdbid"/>' +
          '<book-search available="yes" supportedParams="q"/>' +
          '</searching></caps>',
      )
      return
    }
    if (q.get('t') === 'tvsearch' && !q.get('ep')) {
      res.end(releasesXML([
        // Break the otherwise exact planner tie in favor of the distinct RSS
        // payload. Equal quality/coverage/size leaves selection dependent on
        // candidate-key ordering and can make the auto job reuse pack700.
        automaticSeasonPack,
        item('The.Test.Show.S01.1080p.WEB-DL-E2E', 'pack700', 2_400_000_000, 33),
      ]))
      return
    }
    if (q.get('t') === 'tvsearch') {
      res.end(releasesXML([
        automaticSeasonPack,
      ]))
      return
    }
    // Ebook and audiobook profiles search their own Newznab categories.
    if ((q.get('cat') ?? '').includes('7000')) {
      res.end(releasesXML([
        item('Test Author - The Test Book (2024) EPUB', 'book900', 800000, 21),
      ]))
      return
    }
    if ((q.get('cat') ?? '').includes('3030')) {
      if ((q.get('q') ?? '').toLowerCase().includes('test book')) {
        res.end(releasesXML([
          item('The Test Book by Test Author M4B', 'book900m4b', 180000000, 12),
        ]))
        return
      }
      res.end(releasesXML([
        item('Audio Author - The Test Audiobook M4B', 'book901m4b', 300000000, 9),
      ]))
      return
    }
    // RSS is the automatic acquisition path. Include each fixture kind so
    // manual additional-copy tests can leave the monitored primary untouched,
    // then exercise the normal wanted → RSS → automatic import flow.
    if (q.get('t') === 'search' && !q.get('q')) {
      const movieRelease = torrents.some((torrent) => torrent.hash === 'hash-movie601auto')
        ? item('The.Test.Movie.2024.1080p.WEB-DL.x264-AUTO-REGRAB', 'movie601autoregrab', 4_000_000_000, 50)
        : item('The.Test.Movie.2024.1080p.WEB-DL.x264-AUTO', 'movie601auto', 4_000_000_000, 50)
      console.log(`[fake-arr] RSS fixture requested; book/audio candidates included`)
      res.end(releasesXML([
        movieRelease,
        item('The.Test.Movie.2024.HDCAM.x264-JUNK', 'movie601cam', 900_000_000, 2),
        automaticSeasonPack,
        item('Test Author - The Test Book (2024) EPUB AUTO', 'book900auto', 800_000, 21),
        item('The Test Book by Test Author M4B AUTO', 'book900m4bauto', 180_000_000, 12),
        item('Audio Author - The Test Audiobook M4B AUTO', 'book901m4bauto', 300_000_000, 9),
      ]))
      return
    }
    // movie search: one good candidate, one CAM (rejected by profile rank order)
    res.end(releasesXML([
      item('The.Test.Movie.2024.1080p.WEB-DL.x264-E2E', 'movie601', 4_000_000_000, 50),
      item('The.Test.Movie.2024.HDCAM.x264-JUNK', 'movie601cam', 900_000_000, 2),
    ]))
    return
  }

  // ---- webhook sink (Phase 3 notifier target) ----
  if (url.pathname === '/webhook-sink') {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      try { sinkLog.push(JSON.parse(body)) } catch { /* ignore */ }
      res.end('ok')
    })
    return
  }
  if (url.pathname === '/webhook-sink/log') {
    res.setHeader('content-type', 'application/json')
    res.end(JSON.stringify(sinkLog))
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
      const tags = params.get('tags') ?? ''
      const slug = dlURL.split('/').pop()
      const spec = payloads[slug]
      console.log(`[fake-arr] add slug=${slug} tags=${tags}`)
      if (!spec) {
        res.end('Fails.')
        return
      }
      const dir = join(dlRoot, spec.name)
      mkdirSync(dir, { recursive: true })
      for (const f of spec.files) writeFileSync(join(dir, f), `fake video ${f}`)
      // Re-adding the same torrent retains one hash, as qBittorrent does.
      if (!torrents.some((t) => t.name === spec.name)) {
        torrents.push({ hash: `hash-${slug}`, name: spec.name, content_path: dir, tags })
      }
      res.end('Ok.')
    })
    return
  }
  if (url.pathname === '/api/v2/torrents/info') {
    res.setHeader('content-type', 'application/json')
    res.end(JSON.stringify(torrents.map((t) => ({
      hash: t.hash, name: t.name, progress: 1, state: 'stalledUP',
      save_path: dlRoot, content_path: t.content_path, tags: t.tags ?? '',
    }))))
    return
  }
  res.statusCode = 404
  res.end('not found')
}).listen(PORT, '127.0.0.1', () => console.log(`fake arr services on :${PORT}`))
