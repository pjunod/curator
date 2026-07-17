package api

import (
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/monarr/monarr/web"
)

// fallbackHTML is served when the binary was built without the web UI
// (e.g. `go build` without `make web`). The binary must still run.
const fallbackHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Monarr</title>
<style>
  body { background: #0e1117; color: #e6e9ef; font: 16px/1.6 system-ui, sans-serif;
         display: grid; place-items: center; min-height: 100vh; margin: 0; }
  main { text-align: center; }
  h1 { font-weight: 600; letter-spacing: 0.02em; }
  h1 span { color: #8b7cf6; }
  code { background: #1a1f2b; padding: 0.15em 0.45em; border-radius: 6px; }
  p { color: #9aa3b2; }
</style>
</head>
<body>
<main>
  <h1>mon<span>arr</span></h1>
  <p>The server is running, but this binary was built without the web UI.</p>
  <p>Build it with <code>make build</code> (or <code>cd web && npm ci && npm run build</code>, then rebuild).</p>
  <p>The API is live at <code>/api/v1/system/status</code>.</p>
</main>
</body>
</html>
`

// UIBuilt reports whether the embedded dist contains a real build.
func UIBuilt() bool {
	_, err := fs.Stat(web.Dist, "dist/index.html")
	return err == nil
}

// spaHandler serves the embedded React app: real files when they exist,
// index.html for client-side routes, and a fallback page when the UI was
// not built into this binary.
func (s *Server) spaHandler() http.Handler {
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic("web dist sub fs: " + err.Error()) // embed is broken at build time
	}
	fileServer := http.FileServerFS(dist)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if info, err := fs.Stat(dist, path); err == nil && !info.IsDir() {
				if strings.HasPrefix(path, "assets/") {
					// Vite emits content-hashed asset names; cache forever.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// SPA fallback: unknown paths get index.html so client-side routing
		// works on refresh and deep links.
		idx, err := fs.ReadFile(dist, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			io.WriteString(w, fallbackHTML)
			return
		}
		_, _ = w.Write(idx)
	})
}
