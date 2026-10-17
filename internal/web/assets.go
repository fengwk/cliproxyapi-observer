// Package web exposes the embedded, dependency-free Observer management dashboard.
//
// The dashboard is plain HTML/CSS/JavaScript compiled into the plugin binary via
// go:embed. There is no build step, no runtime dependency and no external network
// access: every asset is served from the same origin as the management API.
package web

import (
	_ "embed"
	"strings"
)

//go:embed ui.html
var uiHTML []byte

//go:embed ui.css
var uiCSS []byte

//go:embed ui.js
var uiJS []byte

//go:embed settings.js
var settingsJS []byte

//go:embed models.js
var modelsJS []byte

// uiBundle is the single public ui.js asset: settings.js stays a separate
// source file for review, but is concatenated ahead of ui.js so the fixed
// /ui.js endpoint serves both without exposing another resource route.
var uiBundle = func() []byte {
	body := make([]byte, 0, len(settingsJS)+len(modelsJS)+2+len(uiJS))
	body = append(body, settingsJS...)
	body = append(body, '\n')
	body = append(body, modelsJS...)
	body = append(body, '\n')
	body = append(body, uiJS...)
	return body
}()

// MIME types used when the host serves the assets. Charts are UTF-8 so the
// Chinese interface renders without a charset guess.
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
	"ui.html": {uiHTML, ContentTypeHTML},
	"ui.css":  {uiCSS, ContentTypeCSS},
	"ui.js":   {uiBundle, ContentTypeJS},
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
