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

// ---- Open Library (books, ADR 0006) — same fake server, distinct paths ----

const routes = {
  '/search/movie': { results: [{ id: 601, title: movie601.title, release_date: movie601.release_date, overview: movie601.overview, poster_path: '' }] },
  '/search/tv': { results: [{ id: 700, name: tv700.name, first_air_date: tv700.first_air_date, overview: tv700.overview, poster_path: '' }] },
  '/movie/601': movie601,
  '/tv/700': tv700,
  '/tv/700/season/1': tv700season1,
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

createServer((req, res) => {
  const { pathname } = new URL(req.url, 'http://x')
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
