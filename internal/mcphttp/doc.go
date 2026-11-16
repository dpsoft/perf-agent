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
// Admission never waits. A body is read only with a reader and its
// Content-Length reserved from the ingress bounds (MaxIngressReaders,
// MaxIngressBytes); otherwise the answer is 503 unread, and a body that
// does not arrive within IngressBodyTimeout loses its connection. Both are
// given back once the body is parsed. The message then takes a place in
// its class (control, page state or calls, see Class), each with its own
// caps, so a full call class never delays a cancel; a full class answers
// 503 at once, naming the cap. A call keeps its place until its handler
// returns, even after its client went away.
//
// The Transport sits behind livehttp.Front and livehttp.MCPGuard, which
// check the Host, refuse browsers, check the bearer token and the headers,
// and bound the body before any of it is read. It opens no connection.
package mcphttp
