// Package web embeds the static front-end assets into the binary.
package web

import "embed"

// Files holds the single-page UI: HTML, CSS and ES modules.
//
//go:embed *.html *.css *.js *.svg
var Files embed.FS
