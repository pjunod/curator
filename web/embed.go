// Package web embeds the built React UI (web/dist) into the binary.
//
// dist/ is produced by `npm run build` (see Makefile target `web`). A bare
// `go build` without a web build still compiles — dist/ always contains at
// least .gitkeep — and the server then serves a fallback page.
package web

import "embed"

// Dist holds the built UI under "dist/".
//
//go:embed all:dist
var Dist embed.FS
