// A minimal fake TMDB v3 server for end-to-end tests. Serves just enough of
// /search and /movie//tv detail endpoints for Monarr's adapter. The real
// adapter's mapping is contract-tested against recorded fixtures in
// internal/adapters/tmdb; this fake exists so E2E can exercise the full
// add-media flow without network access or a real API key.
import { createServer } from 'node:http'

const PORT = Number(process.env.FAKE_TMDB_PORT ?? 7788)

const movie601 = {
  id: 601,
  title: 'The Test Movie',
  overview: 'A movie that exists only inside the e2e suite.',
  release_date: '2024-03-01',
  runtime: 101,
  status: 'Released',
  poster_path: '',
  backdrop_path: '',
  genres: [{ id: 18, name: 'Drama' }],
  imdb_id: 'tt6010001',
  vote_average: 7.6,
  vote_count: 4321,
}

const tv700 = {
  id: 700,
  name: 'The Test Show',
  overview: 'A series that exists only inside the e2e suite.',
  first_air_date: '2020-01-01',
  status: 'Ended',
  poster_path: '',
  backdrop_path: '',
  genres: [{ id: 35, name: 'Comedy' }],
  episode_run_time: [30],
  vote_average: 8.2,
  vote_count: 999,
  seasons: [{ season_number: 1, episode_count: 2, name: 'Season 1' }],
  external_ids: { imdb_id: 'tt7000001', tvdb_id: 700700 },
}

const tv700season1 = {
  season_number: 1,
  episodes: [
    { season_number: 1, episode_number: 1, name: 'Pilot', air_date: '2020-01-01' },
    { season_number: 1, episode_number: 2, name: 'Finale', air_date: '2020-01-08' },
  ],
}

// An umbrella entry: one series whose alternative titles name several
// programmes, which is how TMDB files the Cunk shows. A folder per programme
// therefore points every review row at this one id — the case adoption has to
// describe rather than let the user click into a conflict.
const tv900 = {
  id: 900,
  name: 'Panel Show on...',
  overview: 'One series the provider files several programmes under.',
  first_air_date: '2018-04-03',
  status: 'Ended',
  poster_path: '',
  backdrop_path: '',
  genres: [{ id: 35, name: 'Comedy' }],
  episode_run_time: [30],
  vote_average: 7.1,
  vote_count: 120,
  seasons: [{ season_number: 1, episode_count: 1, name: 'Season 1' }],
  external_ids: { imdb_id: 'tt9000001', tvdb_id: 900900 },
}

const tv900alts = {
  results: [
    { iso_3166_1: 'GB', title: 'Panel Show on Britain' },
    { iso_3166_1: 'GB', title: 'Panel Show on Earth' },
  ],
}

// ---- Open Library (books, ADR 0006) — same fake server, distinct paths ----

const routes = {
  '/search/movie': { results: [{ id: 601, title: movie601.title, release_date: movie601.release_date, overview: movie601.overview, poster_path: '' }] },
  '/search/tv': { results: [{ id: 700, name: tv700.name, first_air_date: tv700.first_air_date, overview: tv700.overview, poster_path: '' }] },
  '/movie/601': movie601,
  '/tv/700': tv700,
  '/tv/700/season/1': tv700season1,
  '/tv/900': tv900,
  '/tv/900/alternative_titles': tv900alts,
  '/tv/900/season/1': {
    season_number: 1,
    episodes: [{ season_number: 1, episode_number: 1, name: 'Only', air_date: '2018-04-03' }],
  },
  '/search.json': {
    numFound: 1,
    docs: [{
      key: '/works/OL900E2EW', title: 'The Test Book',
      author_name: ['Test Author'], first_publish_year: 2024, cover_i: 0,
    }],
  },
  '/works/OL900E2EW.json': {
    title: 'The Test Book',
    description: 'A book that exists only inside the e2e suite.',
    covers: [],
    subjects: ['Testing'],
    first_publish_date: '2024',
    authors: [{ author: { key: '/authors/OL900E2EA' } }],
  },
  '/authors/OL900E2EA.json': { name: 'Test Author' },
  '/works/OL900E2EW/editions.json': {
    entries: [{ publish_date: '2024', isbn_13: ['9781000000001'] }],
  },
}

// ---- TVmaze (the series chain, ADR 0011) — same fake, distinct paths ----
//
// The chain is only consulted for a folder the first provider could not
// place, so for most of this suite the right answer is "nothing here". One
// show exists, to prove a chain match reaches the library.
const tvmazeShows = [
  {
    id: 5100,
    name: 'The Chain Only Show',
    premiered: '2021-06-01',
    status: 'Ended',
    genres: ['Drama'],
    averageRuntime: 42,
    summary: '<p>A series only the second link of the chain knows about.</p>',
    image: { medium: '', original: '' },
    rating: { average: 8.1 },
    externals: { tvrage: null, thetvdb: 510000, imdb: 'tt5100001' },
  },
]

const tvmazeEpisodes = {
  5100: [
    { name: 'Link One', season: 1, number: 1, type: 'regular', airdate: '2021-06-01' },
    { name: 'Link Two', season: 1, number: 2, type: 'regular', airdate: '2021-06-08' },
  ],
}

createServer((req, res) => {
  const { pathname, searchParams } = new URL(req.url, 'http://x')

  // TVmaze: /search/shows, /lookup/shows?thetvdb= (301), /shows/{id}/episodes
  if (pathname === '/search/shows') {
    const q = (searchParams.get('q') ?? '').toLowerCase()
    const hits = tvmazeShows
      .filter((s) => s.name.toLowerCase().includes(q) && q.length > 2)
      .map((show) => ({ score: 1, show }))
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify(hits))
    return
  }
  if (pathname === '/lookup/shows') {
    const show = tvmazeShows.find((s) => String(s.externals.thetvdb) === searchParams.get('thetvdb'))
    if (!show) {
      res.writeHead(404, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ status: 404 }))
      return
    }
    // The real API answers 301 here rather than returning the show inline.
    res.writeHead(301, { location: `/shows/${show.id}` })
    res.end()
    return
  }
  const tvmazeShow = /^\/shows\/(\d+)$/.exec(pathname)
  if (tvmazeShow) {
    const show = tvmazeShows.find((s) => String(s.id) === tvmazeShow[1])
    res.writeHead(show ? 200 : 404, { 'content-type': 'application/json' })
    res.end(JSON.stringify(show ?? { status: 404 }))
    return
  }
  const tvmazeEps = /^\/shows\/(\d+)\/episodes$/.exec(pathname)
  if (tvmazeEps) {
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify(tvmazeEpisodes[tvmazeEps[1]] ?? []))
    return
  }
  // /search/tv is the one route that has to read the query: the umbrella
  // series must be findable without turning up beside The Test Show in the
  // add-media flow, where a second result would make "Add" ambiguous.
  if (pathname === '/search/tv' && /panel show/i.test(searchParams.get('query') ?? '')) {
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify({
      results: [{
        id: 900, name: tv900.name, first_air_date: tv900.first_air_date,
        overview: tv900.overview, poster_path: '',
      }],
    }))
    return
  }
  const body = routes[pathname]
  if (!body) {
    res.writeHead(404, { 'content-type': 'application/json' })
    res.end(JSON.stringify({ status_message: 'not found' }))
    return
  }
  res.writeHead(200, { 'content-type': 'application/json' })
  res.end(JSON.stringify(body))
}).listen(PORT, '127.0.0.1', () => {
  console.log(`fake tmdb listening on :${PORT}`)
})
