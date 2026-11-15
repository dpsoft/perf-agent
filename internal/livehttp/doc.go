// Package livehttp is the HTTP boundary of perf-agent-mcp's live page. It
// knows nothing about profiles; it only decides which requests reach the
// page's handlers.
//
// Its promise:
//   - Loopback only. Listen binds a loopback IP literal and nothing else, and
//     Guard refuses any Host that is not localhost, an IPv4 loopback literal
//     or [::1], which defeats DNS rebinding.
//   - Read only. Guard passes GET and HEAD and refuses every other method,
//     so a browser can read the page but never change server state. No
//     response carries an Access-Control-* header.
//   - No client. The package listens; it never dials, resolves a name or
//     makes an HTTP request.
//
// Every URL lives under /s/<token>/, where the token is a per-run secret of
// 256 random bits compared in constant time. A wrong or missing token gets
// 404, so a probe learns nothing from the status. Cross-site requests are
// refused through Origin and Sec-Fetch-Site, except a top-level navigation
// to the shell route, which holds no data and whose own requests are all
// same-origin.
package livehttp
