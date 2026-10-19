// Package roundtable embeds the static web client.
package roundtable

import "embed"

// WebFS holds index.html, the js/ ES modules, style.css and vendored libraries.
// A directory pattern embeds its subdirectories too, but skips names that start
// with "." or "_".
//
//go:embed web
var WebFS embed.FS
