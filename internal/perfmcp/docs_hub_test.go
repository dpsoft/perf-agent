package perfmcp

import (
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// wantDefault fails unless cell, a docs table's default cell, starts with
// the default v, written as a Go duration or a decimal integer.
func wantDefault(t *testing.T, name, cell string, v reflect.Value) {
	t.Helper()
	first, _, _ := strings.Cut(cell, " ")
	if v.Type() == reflect.TypeFor[time.Duration]() {
		if d, err := time.ParseDuration(first); err != nil || d != time.Duration(v.Int()) {
			t.Errorf("docs/mcp.md gives `%s` the default %q, the server %v", name, cell, time.Duration(v.Int()))
		}
		return
	}
	if n, err := strconv.ParseInt(first, 10, 64); err != nil || n != v.Int() {
		t.Errorf("docs/mcp.md gives `%s` the default %q, the server %d", name, cell, v.Int())
	}
}

// docs/mcp.md has a row for every Hub limit and for MaxEngineWait, whose
// default cell starts with the server's default.
func TestDocsListEveryHubLimit(t *testing.T) {
	docs := readDocs(t)
	def := DefaultLimits()
	v := reflect.ValueOf(def.Hub)
	if v.NumField() < 10 {
		t.Fatalf("only %d Hub limits", v.NumField())
	}
	for i := range v.NumField() {
		name := "Hub." + v.Type().Field(i).Name
		row := tableRow(docs, name)
		if len(row) < 2 {
			t.Errorf("docs/mcp.md has no limits row for `%s`", name)
			continue
		}
		wantDefault(t, name, row[0], v.Field(i))
	}
	row := tableRow(docs, "MaxEngineWait")
	if len(row) < 1 {
		t.Fatal("docs/mcp.md has no limits row for `MaxEngineWait`")
	}
	wantDefault(t, "MaxEngineWait", row[0], reflect.ValueOf(def.MaxEngineWait))
}

// hubPort is perf-agent-hub's default port, read from its source so the
// docs cannot drift from it.
func hubPort(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../cmd/perf-agent-hub/main.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^const DefaultPort = (\d+)$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("cmd/perf-agent-hub/main.go declares no DefaultPort")
	}
	return string(m[1])
}

// docs/mcp.md shows both clients' configurations for the hub's default
// port, with the URL's port in the helper, and warns about a port the hub
// is not on.
func TestDocsShowBothClientConfigs(t *testing.T) {
	docs := readDocs(t)
	port := hubPort(t)
	url := "http://127.0.0.1:" + port + "/mcp"
	helper := "perf-agent-hub token --port " + port
	for _, want := range []string{
		`"type": "http"`,
		`"url": "` + url + `"`,
		`"headersHelper": "` + helper + `"`,
		`url = "` + url + `"`,
		`http_headers_helper = "` + helper + `"`,
		"`--port N` | The port, default " + port + ".",
		"**A port the hub is not on.** Any local user can listen on a loopback port the hub is not using.",
		"a client whose URL names such a port talks to an impostor",
		"a prompt-injection channel",
		"Keep the client's URL and `--port` in step",
		"check `perf-agent-hub url`",
		"**Do not configure both in one session.**",
		"never `localhost`",
	} {
		if !strings.Contains(docs, want) {
			t.Errorf("docs/mcp.md does not contain %q", want)
		}
	}
	// No other port is named for the hub.
	for _, m := range regexp.MustCompile(`127\.0\.0\.1:(\d+)`).FindAllStringSubmatch(docs, -1) {
		if m[1] != port {
			t.Errorf("docs/mcp.md names 127.0.0.1:%s; the hub's port is %s", m[1], port)
		}
	}
	for _, m := range regexp.MustCompile(`token --port (\d+)`).FindAllStringSubmatch(docs, -1) {
		if m[1] != port {
			t.Errorf("docs/mcp.md's helper names port %s; the hub's port is %s", m[1], port)
		}
	}
}

// Nothing tells an agent or a user to reach the hub from elsewhere: it
// listens on loopback only, and has no address flag.
func TestDocsNeverDescribeRemoteUse(t *testing.T) {
	skill, err := os.ReadFile("../../skills/perf-agent-mcp/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"docs/mcp.md": readDocs(t),
		"the guide":   guideText,
		"the skill":   string(skill),
	} {
		lower := strings.ToLower(text)
		for _, word := range []string{"ssh", "-l ", "0.0.0.0", "--addr", "tunnel", "port forward"} {
			if strings.Contains(lower, word) {
				t.Errorf("%s mentions %q", name, word)
			}
		}
	}
}
