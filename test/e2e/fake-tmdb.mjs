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

// ---- Discover rows (ADR 0015) ----
//
// One entry per row, with ids distinct from the add-media fixtures on
// purpose: a spec that adds from a discover row must not be re-adding the
// movie the library specs already added, or "in library" would be true
// before anything was clicked.

const discoverMovie = (id, title, year) => ({
  id, title, original_title: title, release_date: `${year}-05-01`,
  overview: `${title} is a discover fixture.`, poster_path: '',
})
const discoverShow = (id, name, year) => ({
  id, name, original_name: name, first_air_date: `${year}-05-01`,
  overview: `${name} is a discover fixture.`, poster_path: '',
})

const discoverRoutes = {
  '/trending/movie/week': { results: [discoverMovie(6101, 'Trending Test Movie', 2026)] },
  '/movie/now_playing': { results: [discoverMovie(6102, 'In Theaters Test Movie', 2026)] },
  '/movie/upcoming': { results: [discoverMovie(6103, 'Upcoming Test Movie', 2027)] },
  '/movie/popular': { results: [discoverMovie(6104, 'Popular Test Movie', 2025)] },
  '/movie/top_rated': { results: [discoverMovie(6105, 'Top Rated Test Movie', 1994)] },
  '/trending/tv/week': { results: [discoverShow(7101, 'Trending Test Show', 2026)] },
  '/tv/on_the_air': { results: [discoverShow(7102, 'On The Air Test Show', 2026)] },
  '/tv/popular': { results: [discoverShow(7103, 'Popular Test Show', 2025)] },
  '/tv/top_rated': { results: [discoverShow(7104, 'Top Rated Test Show', 1999)] },
}

// Detail endpoints. /movie/6101 is what the spec adds. The other two exist so
// Trakt's artwork hydration has somewhere to land — and Trakt's box-office
// entry deliberately points at an id with NO detail route, which is the
// hydration-failure path: that card should keep its title and lose only its
// picture.
const discoverDetail = {
  '/movie/6101': {
    ...movie601,
    id: 6101, title: 'Trending Test Movie', release_date: '2026-05-01', imdb_id: 'tt6101001',
  },
  '/movie/6104': {
    ...movie601,
    id: 6104, title: 'Popular Test Movie', release_date: '2025-05-01',
    poster_path: '/hydrated.jpg', imdb_id: 'tt6104001',
  },
  '/tv/7103': {
    ...tv700,
    id: 7103, name: 'Popular Test Show', first_air_date: '2025-05-01',
    poster_path: '/hydrated-show.jpg', seasons: [],
  },
}

// Five movies sharing one release date, so a month cell overflows its chip
// budget and the "+N more" day panel has something to open. Nothing else
// reaches them: they are added by the calendar spec, by id.
const crowdedDay = '2020-01-15'
const crowdedMovies = Object.fromEntries(
  ['Alpha', 'Bravo', 'Charlie', 'Delta', 'Echo'].map((name, i) => [
    `/movie/${6200 + i}`,
    {
      ...movie601,
      id: 6200 + i,
      title: `Crowded ${name}`,
      release_date: crowdedDay,
      imdb_id: `tt620000${i}`,
    },
  ]),
)

// Trakt, on the same port — its paths collide with nothing else here. The
// entities carry TMDB ids and no artwork, exactly as the real API does, so
// the suite exercises the Discover service's hydration path.
const traktRoutes = {
  '/movies/trending': [
    { watchers: 120, movie: { title: 'Trakt Trending Film', year: 2026, overview: 'Being watched.', ids: { tmdb: 6104, tvdb: 0 } } },
  ],
  '/shows/trending': [
    { watchers: 40, show: { title: 'Trakt Trending Show', year: 2026, overview: 'Also watched.', ids: { tmdb: 7103, tvdb: 71030 } } },
  ],
  '/movies/anticipated': [],
  '/shows/anticipated': [],
  '/movies/boxoffice': [
    { revenue: 9000000, movie: { title: 'Trakt Box Office Film', year: 2026, overview: 'Grossed.', ids: { tmdb: 6105, tvdb: 0 } } },
  ],
}

// ---- Open Library (books, ADR 0006) — same fake server, distinct paths ----

const routes = {
  ...discoverRoutes,
  ...discoverDetail,
  ...crowdedMovies,
  ...traktRoutes,
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
  '/works/OL901A2AW.json': {
    title: 'The Test Audiobook',
    description: 'A multipart audiobook that exists only inside the e2e suite.',
    covers: [],
    subjects: ['Testing'],
    first_publish_date: '2025',
    authors: [{ author: { key: '/authors/OL901A2AA' } }],
  },
  '/authors/OL901A2AA.json': { name: 'Audio Author' },
  '/works/OL901A2AW/editions.json': {
    entries: [{ publish_date: '2025', isbn_13: ['9781000000002'] }],
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
    // Deliberately schedule-less: half the calendar's rows are date-only in
    // any real library (streaming drops), and both render paths need one.
  },
  {
    // The TMDB series ("The Test Show", tv700) as TVmaze knows it. It exists
    // ONLY so the airing enrichment (ADR 0016) has a schedule to find —
    // `searchable: false` keeps it out of /search/shows, because the chain
    // specs assert what a search returns and this show is not theirs.
    id: 5700,
    searchable: false,
    name: 'The Test Show',
    premiered: '2020-01-01',
    status: 'Ended',
    genres: ['Comedy'],
    averageRuntime: 30,
    summary: '<p>The series the suite imports.</p>',
    image: { medium: '', original: '' },
    rating: { average: 8.2 },
    externals: { tvrage: null, thetvdb: 700700, imdb: 'tt7000001' },
    schedule: { time: '21:00', days: ['Wednesday'] },
    network: { name: 'E2E One', country: { name: 'United States', code: 'US', timezone: 'America/New_York' } },
    webChannel: null,
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

  if (pathname === '/search.json' && /audio/i.test(searchParams.get('q') ?? '')) {
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify({
      numFound: 1,
      docs: [{
        key: '/works/OL901A2AW', title: 'The Test Audiobook',
        author_name: ['Audio Author'], first_publish_year: 2025, cover_i: 0,
      }],
    }))
    return
  }

  // TVmaze: /search/shows, /lookup/shows?thetvdb= (301), /shows/{id}/episodes
  if (pathname === '/search/shows') {
    const q = (searchParams.get('q') ?? '').toLowerCase()
    const hits = tvmazeShows
      .filter((s) => s.searchable !== false && s.name.toLowerCase().includes(q) && q.length > 2)
      .map((show) => ({ score: 1, show }))
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify(hits))
    return
  }
  if (pathname === '/lookup/shows') {
    // Keyed on TheTVDB id first, IMDb second — both are real TVmaze lookup
    // keys and the airing provider falls back to the second one.
    const tvdb = searchParams.get('thetvdb')
    const imdb = searchParams.get('imdb')
    const show = tvmazeShows.find((s) =>
      tvdb ? String(s.externals.thetvdb) === tvdb : imdb ? s.externals.imdb === imdb : false,
    )
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
