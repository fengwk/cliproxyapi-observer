// Package web exposes the embedded Observer management dashboard.
package web

import (
	_ "embed"
	"strings"
)

//go:embed dist/ui.html
var uiHTML []byte

//go:embed dist/ui.css
var uiCSS []byte

//go:embed dist/ui.js
var uiJS []byte

//go:embed dist/theme.js
var themeJS []byte

// MIME types used when the host serves the assets.
const (
	ContentTypeHTML = "text/html; charset=utf-8"
	ContentTypeCSS  = "text/css; charset=utf-8"
	ContentTypeJS   = "text/javascript; charset=utf-8"
)

type asset struct {
	body        []byte
	contentType string
}

var assets = map[string]asset{
	"ui.html":  {uiHTML, ContentTypeHTML},
	"ui.css":   {uiCSS, ContentTypeCSS},
	"ui.js":    {uiJS, ContentTypeJS},
	"theme.js": {themeJS, ContentTypeJS},
}

// Asset returns the embedded dashboard asset identified by name together with
// its MIME type. The returned bytes are a copy so callers can never mutate the
// binary-embedded originals. The boolean reports whether the asset exists.
func Asset(name string) ([]byte, string, bool) {
	a, ok := assets[strings.TrimSpace(name)]
	if !ok {
		return nil, "", false
	}
	out := make([]byte, len(a.body))
	copy(out, a.body)
	return out, a.contentType, true
}
