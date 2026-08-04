import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'

// Dev: `npm run dev` serves on :5173 and proxies API calls to the Go server
// (`make dev-api`) on :7676. Prod: `npm run build` → dist/, embedded into the
// binary via go:embed.
//
// PWA: the service worker precaches the built shell and caches TMDB poster
// images; the API is deliberately network-only — stale library/queue data
// pretending to be live is worse than an offline banner. New builds
// auto-update on next load.
export default defineConfig({
  plugins: [
    react(),
    VitePWA({
      registerType: 'autoUpdate',
      includeAssets: ['apple-touch-icon.png'],
      manifest: {
        name: 'Noirr Curator',
        short_name: 'Curator',
        description: 'Find, evaluate, import, and organize movies, series, and books.',
        start_url: '/',
        display: 'standalone',
        background_color: '#0a0a0c',
        theme_color: '#0a0a0c',
        icons: [
          { src: '/pwa-192.png', sizes: '192x192', type: 'image/png' },
          { src: '/pwa-512.png', sizes: '512x512', type: 'image/png' },
          { src: '/pwa-maskable-512.png', sizes: '512x512', type: 'image/png', purpose: 'maskable' },
        ],
      },
      workbox: {
        // SPA routes fall back to the shell; server-side surfaces never do.
        navigateFallback: 'index.html',
        navigateFallbackDenylist: [/^\/api\//, /^\/sonarr\//, /^\/radarr\//, /^\/metrics/],
        runtimeCaching: [
          {
            urlPattern: /^https:\/\/image\.tmdb\.org\/.*/i,
            handler: 'CacheFirst',
            options: {
              cacheName: 'tmdb-posters',
              expiration: { maxEntries: 300, maxAgeSeconds: 60 * 60 * 24 * 30 },
              cacheableResponse: { statuses: [0, 200] },
            },
          },
          {
            urlPattern: /^https:\/\/covers\.openlibrary\.org\/.*/i,
            handler: 'CacheFirst',
            options: {
              cacheName: 'book-covers',
              expiration: { maxEntries: 300, maxAgeSeconds: 60 * 60 * 24 * 30 },
              cacheableResponse: { statuses: [0, 200] },
            },
          },
        ],
      },
    }),
  ],
  server: {
    proxy: {
      '/api': 'http://localhost:7676',
    },
  },
})
