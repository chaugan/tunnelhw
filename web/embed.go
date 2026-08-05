// Package web embeds the zero-build web UI assets served by the agent's
// localhost UI (ARCHITECTURE.md §10). Vanilla HTML/CSS/JS only — no build
// step, preserving the single-binary install story.
package web

import "embed"

// FS holds the UI's static files.
//
//go:embed index.html app.js style.css
var FS embed.FS
