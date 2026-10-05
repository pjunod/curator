import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { getDekkooFeed } from './api'
import './DekkooFeed.css'

export function DekkooFeed() {
  const feed = useQuery({
    queryKey: ['dekkoo-feed'],
    queryFn: getDekkooFeed,
    staleTime: 5 * 60_000,
    retry: false,
  })
  const [title, setTitle] = useState('')
  const [kind, setKind] = useState<'movie' | 'series'>('movie')
  return (
    <section className="dekkoo-feed" aria-labelledby="dekkoo-heading">
      <h2 id="dekkoo-heading">Gay films and series from Dekkoo</h2>
      <p className="muted">
        Recent articles from Gay Movies, Gay Series, Gay Romance, Gay Comedy and Gay Short Films. An
        article may introduce several titles.
      </p>
      <div className="dekkoo-search">
        <label>
          Title to find
          <input
            type="search"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            placeholder="Enter a title from an article"
          />
        </label>
        <label>
          Media type
          <select value={kind} onChange={(e) => setKind(e.target.value as 'movie' | 'series')}>
            <option value="movie">Movie</option>
            <option value="series">Series</option>
          </select>
        </label>
        {title.trim() ? (
          <Link className="btn-accent" to="/add" search={{ kind, q: title.trim() }}>
            Find in Curator
          </Link>
        ) : (
          <button className="btn" disabled>
            Find in Curator
          </button>
        )}
      </div>
      {feed.isPending && <p role="status">Loading Dekkoo articles…</p>}
      {feed.isError && (
        <div className="banner warning" role="alert">
          <p>Could not refresh the Dekkoo feed. Try again shortly.</p>
          <button className="btn" onClick={() => void feed.refetch()}>
            Try again
          </button>
        </div>
      )}
      {feed.data && (
        <>
          <div className="dekkoo-status">
            <p className="muted">
              {feed.data.stale
                ? 'Dekkoo is temporarily unavailable. Showing articles last refreshed '
                : 'Last refreshed '}
              {new Date(feed.data.fetchedAt).toLocaleString()}.
            </p>
            <a href={feed.data.url} target="_blank" rel="noopener noreferrer">
              Combined RSS feed
            </a>
          </div>
          {feed.data.items.length === 0 && <p>No articles in these categories right now.</p>}
          <div className="dekkoo-grid">
            {feed.data.items.map((item) => (
              <article className="dekkoo-article" key={item.url}>
                <div className="dekkoo-categories">
                  {item.categories.map((category) => (
                    <span className="pill" key={category}>
                      {category}
                    </span>
                  ))}
                </div>
                <h3>
                  <a href={item.url} target="_blank" rel="noopener noreferrer">
                    {item.title}
                  </a>
                </h3>
                {item.publishedAt && (
                  <time className="muted" dateTime={item.publishedAt}>
                    {new Date(item.publishedAt).toLocaleDateString()}
                  </time>
                )}
                <p>{item.summary}</p>
                <a href={item.url} target="_blank" rel="noopener noreferrer">
                  Read on Dekkoo ↗
                </a>
              </article>
            ))}
          </div>
        </>
      )}
    </section>
  )
}
