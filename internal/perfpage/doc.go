// Package perfpage serves perf-agent's live page over HTTP: the shell and
// its assets, the state, the event stream and the routes of each
// publication, below internal/livehttp's guard. It reads a
// perfmcp.Server only through PageSnapshot, so a request never changes
// server state, and it computes nothing itself: the models, frames and
// exports are the engine's. Only the perf-agent-hub binary links it.
package perfpage
