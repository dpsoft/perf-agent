// Package capture runs the user's setcap'd perf-agent binary as a
// supervised child, for perf-agent-mcp's capture tools. It knows nothing
// about the MCP transport.
//
// The server itself never holds or raises a capability: it verifies the
// binary without running it (its security.capability xattr, its mount, its
// mode and its Go build information) and refuses to capture when it runs
// as root, with capabilities, or under no_new_privs.
//
// Capture is Linux only. Elsewhere Supported is false and every entry
// point returns a disabled error.
package capture
