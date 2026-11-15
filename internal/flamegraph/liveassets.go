package flamegraph

import (
	"bytes"
	_ "embed"
	"sync"
)

//go:embed livepage.html
var livePageShell string

//go:embed live.css
var liveOnlyCSS string

//go:embed live.js
var liveScript string

type liveFile struct {
	body        []byte
	contentType string
}

// liveAssets are the live page's files by name, built once: the shell, a
// stylesheet made of the explained page's, the flame graph palette and
// the few rules only the live page has, the shared panel and the page's
// script.
var liveAssets = sync.OnceValue(func() map[string]liveFile {
	const js = "text/javascript; charset=utf-8"
	return map[string]liveFile{
		"":                {[]byte(livePageShell), "text/html; charset=utf-8"},
		"assets/live.css": {[]byte(explainedCSS() + paletteCSS + jitterCSS + liveOnlyCSS), "text/css; charset=utf-8"},
		"assets/panel.js": {[]byte(explainPanelScript), js},
		"assets/live.js":  {[]byte(liveScript), js},
	}
})

// LiveAsset returns the live page's file name, as the page names it
// relative to its own URL ("" is the shell), and its content type. The
// files are fixed: they hold no data, and the page fetches everything it
// shows. Each call returns a copy of the same bytes. Any other name is not
// ok.
func LiveAsset(name string) (body []byte, contentType string, ok bool) {
	a, ok := liveAssets()[name]
	if !ok {
		return nil, "", false
	}
	return bytes.Clone(a.body), a.contentType, true
}
