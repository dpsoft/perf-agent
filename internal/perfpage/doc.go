// Package perfpage serves perf-agent's live page over HTTP: the shell and
// its assets, the state, the event stream and the routes of each
// publication, below internal/livehttp's guard, and the page's reports. Its
// routes read a perfmcp.Server only through PageSnapshot, so a GET never
// changes server state; its report handler changes only the inbox, through
// Server.Report. It computes nothing itself: the models, frames and
// exports are the engine's. Only the perf-agent-hub binary links it.
package perfpage
