// Package roundtable embeds the static web client.
package roundtable

import "embed"

// WebFS holds index.html, app.js, style.css and vendored libraries.
//
//go:embed web
var WebFS embed.FS
