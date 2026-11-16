// Package livehttp is the HTTP boundary of the perf-agent-mcp family's live
// page and of the hub's MCP endpoint. It knows nothing about profiles or MCP
// messages; it only decides which requests reach the handlers.
//
// Its promise:
//   - Loopback only. Listen binds a loopback IP literal and nothing else, and
//     Guard and Front refuse any Host that is not localhost, an IPv4
//     loopback literal or [::1], with a port, which defeats DNS rebinding.
//   - Reads, and two writes. Guard passes GET and HEAD and refuses every
//     other method. The only other requests served are the hub's, through
//     Front: POST (and a body-less DELETE that ends a session) on /mcp,
//     behind MCPGuard, and POST /s/<token>/inbox, behind InboxGuard. MCPGuard
//     needs the MCP bearer token and refuses any request carrying Origin,
//     Sec-Fetch-Site or Sec-Fetch-Dest, so no browser reaches it. InboxGuard
//     admits only the page's own same-origin requests. No response carries
//     an Access-Control-* header.
//   - No client. The package listens; it never dials, resolves a name or
//     makes an HTTP request.
//
// Every page URL lives under /s/<token>/, where the token is a per-run secret
// of 256 random bits compared in constant time. A wrong or missing token gets
// 404, so a probe learns nothing from the status. Cross-site requests are
// refused through Origin and Sec-Fetch-Site, except a top-level navigation
// to the shell route, which holds no data and whose own requests are all
// same-origin. Every guard check runs before any body byte is read, so a
// request a check refuses is answered from its headers alone.
package livehttp
