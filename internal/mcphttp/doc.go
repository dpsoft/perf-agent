// Package mcphttp serves MCP over the Streamable HTTP transport, JSON only,
// through mcpstdio's per-message API, so an HTTP client is answered
// exactly as a stdio client is.
//
// One endpoint takes POST and DELETE. A POST carries one JSON-RPC message:
// a request gets 200 with one application/json answer, a notification or a
// client's response gets 202 with no body, and a batch or a message that
// does not parse gets 400. The server never opens an event stream, sends
// no progress and makes no request of its own.
//
// Sessions are protocol state only (mcpstdio.Session): initialize mints
// one, a bounded table keeps them, least recently used first out, and
// nothing durable is keyed by one. A session holds the ids of its calls in
// flight, so notifications/cancelled and DELETE end only that session's
// calls.
//
// A call's context derives from Options.Lifetime, never from the HTTP
// request: a client that closes its connection does not cancel the call,
// and Close ends every call at once. Deadlines are per phase: the body is
// read under IngressBodyTimeout, the wait for the handler has none, and the
// answer is written under WriteBound.
//
// The Transport sits behind livehttp.Front and livehttp.MCPGuard, which
// check the Host, refuse browsers, check the bearer token and the headers,
// and bound the body before any of it is read. It opens no connection.
package mcphttp
