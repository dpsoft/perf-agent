package livehttp

import (
	"crypto/subtle"
	"net/http"
	"slices"
	"strings"
	"time"
)

// refuseReadWindow is how long a refused request's body may keep arriving
// before the connection is dropped.
const refuseReadWindow = 100 * time.Millisecond

// clearSiteData is the Clear-Site-Data value that clears the origin's
// storage: its service workers, caches and web storage, not its cookies.
const clearSiteData = `"storage"`

// Front is the one handler of the hub's listener. It sets the security
// headers on every answer, refuses with 403 a Host that is not this machine
// (DNS rebinding), and routes by path:
//   - /mcp, any method, to mcp;
//   - POST /s/<page>/inbox to inbox;
//   - any other /s/<page>[/…] to pages;
//   - anything else, a wrong or missing page token included, gets 404.
//
// Before routing, a service worker's script fetch (Service-Worker: script,
// or Sec-Fetch-Dest: serviceworker) gets 404 on any path, /mcp included.
// Every 404 it answers, and every answer on the shell's route, also
// clears the origin's storage (clearSiteData). The page keeps nothing
// there; a service worker that a program squatting on the port registered
// while the hub was down does live there. The browser's update check of
// that worker asks the real hub for its script, gets a 404 and the worker
// is cleared; so is it when the shell itself reaches the hub.
//
// The page token is compared here in constant time as well as in the guards
// below, so a wrong token gets the same 404 whatever the method or body.
func Front(page Token, mcp, inbox, pages http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		if !LoopbackHost(r.Host) {
			refuse(w, http.StatusForbidden, "bad Host")
			return
		}
		if workerScriptFetch(r.Header) {
			w.Header().Set("Clear-Site-Data", clearSiteData)
			refuse(w, http.StatusNotFound, "not found")
			return
		}
		if r.URL.Path == "/mcp" {
			mcp.ServeHTTP(w, r)
			return
		}
		seg, after, ok := pageSegment(r.URL.Path)
		if !ok || !samePageToken(seg, page) {
			w.Header().Set("Clear-Site-Data", clearSiteData)
			refuse(w, http.StatusNotFound, "not found")
			return
		}
		if after == "" {
			w.Header().Set("Clear-Site-Data", clearSiteData)
		}
		if r.Method == http.MethodPost && after == "inbox" {
			inbox.ServeHTTP(w, r)
			return
		}
		pages.ServeHTTP(w, r)
	})
}

// workerScriptFetch reports a browser's fetch of a service worker's script,
// to register the worker or to check it for an update. The hub serves no
// worker, so such a fetch, on any path, can only be for a worker someone
// else registered on this origin.
func workerScriptFetch(h http.Header) bool {
	return slices.Contains(h.Values("Service-Worker"), "script") ||
		slices.Contains(h.Values("Sec-Fetch-Dest"), "serviceworker")
}

// pageSegment splits /s/<seg>[/<after>] into seg and after.
func pageSegment(path string) (seg, after string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/s/")
	if !ok {
		return "", "", false
	}
	seg, after, _ = strings.Cut(rest, "/")
	return seg, after, true
}

// samePageToken compares seg with the page token in constant time. An empty
// token matches nothing.
func samePageToken(seg string, page Token) bool {
	return seg != "" && page != "" && subtle.ConstantTimeCompare([]byte(seg), []byte(page)) == 1
}

func setSecurityHeaders(h http.Header) {
	for _, kv := range securityHeaders {
		h.Set(kv[0], kv[1])
	}
}

// refuse answers code from the request's headers alone, without reading any
// body. After the handler returns, net/http drains an unread body with no
// deadline, because the server sets no ReadTimeout (it would end streams).
// So the body gets refuseReadWindow to arrive and the connection is closed.
// msg is a fixed text: a refusal never repeats what the client sent.
func refuse(w http.ResponseWriter, code int, msg string) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(refuseReadWindow))
	w.Header().Set("Connection", "close")
	http.Error(w, msg, code)
}
