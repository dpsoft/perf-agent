package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// procSocket is one row of /proc/<pid>/net/{tcp,tcp6,udp,udp6}.
type procSocket struct {
	table         string
	local, remote netip.AddrPort
	listening     bool
}

// procSockets reads pid's socket tables, keyed by inode. The tables list
// every socket of the network namespace; the caller picks the process's own
// by the inodes of its descriptors.
func procSockets(t *testing.T, pid int) map[string]procSocket {
	t.Helper()
	out := map[string]procSocket{}
	for _, table := range []string{"tcp", "tcp6", "udp", "udp6"} {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, table))
		if err != nil {
			if os.IsNotExist(err) {
				continue // no IPv6 in this kernel
			}
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		for _, line := range lines[1:] {
			f := strings.Fields(line)
			if len(f) < 10 {
				t.Fatalf("%s: short line %q", table, line)
			}
			out[f[9]] = procSocket{
				table: table, local: procAddr(t, f[1]), remote: procAddr(t, f[2]),
				// 0A is TCP_LISTEN.
				listening: strings.HasPrefix(table, "tcp") && f[3] == "0A",
			}
		}
	}
	return out
}

// procAddr parses an address as the kernel prints it: the IP as 32-bit
// words in host byte order, each in hex, then the port in hex.
func procAddr(t *testing.T, s string) netip.AddrPort {
	t.Helper()
	ipHex, portHex, ok := strings.Cut(s, ":")
	port, err := strconv.ParseUint(portHex, 16, 16)
	if !ok || err != nil || (len(ipHex) != 8 && len(ipHex) != 32) {
		t.Fatalf("socket address %q", s)
	}
	raw := make([]byte, 0, 16)
	for i := 0; i < len(ipHex); i += 8 {
		w, err := strconv.ParseUint(ipHex[i:i+8], 16, 32)
		if err != nil {
			t.Fatalf("socket address %q: %v", s, err)
		}
		raw = binary.NativeEndian.AppendUint32(raw, uint32(w))
	}
	ip, _ := netip.AddrFromSlice(raw)
	return netip.AddrPortFrom(ip.Unmap(), uint16(port))
}

// socketInodes are the inodes of pid's socket descriptors.
func socketInodes(t *testing.T, pid int) []string {
	t.Helper()
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		link, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // closed since ReadDir
		}
		if inode, ok := strings.CutPrefix(link, "socket:["); ok {
			out = append(out, strings.TrimSuffix(inode, "]"))
		}
	}
	return out
}

// TestEverySocketIsLoopback runs perf-agent-hub as go build makes it,
// drives it as agents and a browser do, and checks every socket the
// process holds: each is TCP on 127.0.0.1, listening or connected to a
// loopback peer. An agent speaks MCP over HTTP (initialize, open,
// page_explain, page_state, DELETE), and the page is read (the shell, a
// model, the state), streamed and reported to. After SIGTERM the port is
// closed.
func TestEverySocketIsLoopback(t *testing.T) {
	p := startHub(t, hubOptions{built: true})
	c := p.client("claude")
	o := c.open("cpu.pb.gz")
	pub := c.publish(o)
	c.call("page_state", map[string]any{})

	pageURL := p.pageURL()
	page := strings.TrimPrefix(pageURL, c.origin)
	for _, route := range []string{"", "state", "pub/" + pub + "/model"} {
		if status, body := c.get(page + route); status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", redact(page+route), status, body)
		}
	}
	// The browser's event stream stays open while the sockets are read.
	stream, err := (&http.Client{}).Get(pageURL + "events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()
	if line, err := bufio.NewReader(stream.Body).ReadString('\n'); err != nil || line != "event: state\n" {
		t.Fatalf("stream: %q (%v)", line, err)
	}
	if status := postReport(t, pageURL, map[string]string{"kind": "select", "publication": pub, "node": c.nodeID(o, "a"), "focus": ""}); status != http.StatusNoContent {
		t.Fatalf("POST inbox: %d, want 204", status)
	}
	c.delete()

	pid := p.cmd.Process.Pid
	sockets := procSockets(t, pid)
	inodes := socketInodes(t, pid)
	// A socket made after the tables were read is missing from them: read
	// them once more before calling it a socket of another kind, and skip
	// one the process no longer holds.
	if slices.ContainsFunc(inodes, func(i string) bool { _, ok := sockets[i]; return !ok }) {
		sockets = procSockets(t, pid)
		held := socketInodes(t, pid)
		inodes = slices.DeleteFunc(inodes, func(i string) bool {
			_, ok := sockets[i]
			return !ok && !slices.Contains(held, i)
		})
	}
	loopback := netip.MustParseAddr(host)
	var listening, connected int
	for _, inode := range inodes {
		s, ok := sockets[inode]
		switch {
		case !ok:
			t.Errorf("socket %s is not in the TCP or UDP tables: not a TCP socket", inode)
			continue
		case strings.HasPrefix(s.table, "udp"):
			t.Errorf("socket %s is UDP (%s, local %v)", inode, s.table, s.local)
		case s.local.Addr() != loopback:
			t.Errorf("socket %s is bound to %v, not %v", inode, s.local, loopback)
		case s.listening:
			listening++
		case !s.remote.Addr().IsLoopback():
			t.Errorf("socket %s (local %v) is connected to %v, not loopback", inode, s.local, s.remote)
		default:
			connected++
		}
	}
	// The stream's connection is open, so the check saw the listener and
	// at least that connection.
	if listening != 1 || connected < 1 {
		t.Errorf("%d listening and %d connected sockets among %d, want 1 and at least 1", listening, connected, len(inodes))
	}

	p.stop(syscall.SIGTERM, 5*time.Second)
	addr := net.JoinHostPort(host, strconv.Itoa(p.port))
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Errorf("%s still accepts connections after the hub exited", addr)
	}
}
