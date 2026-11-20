package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/stdio_transcript.golden")

// goldenScript is a representative stdio session: each line is sent, and
// every answer is read before the next is sent, so the bytes are stable.
var goldenScript = []string{
	`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`,
	`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"golden"}}}`,
	`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
	`{"jsonrpc":"2.0","id":"s4","method":"resources/list"}`,
	`{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"mem://r"}}`,
	`{"jsonrpc":"2.0","id":6,"method":"resources/read","params":{"uri":"mem://nope"}}`,
	`{"jsonrpc":"2.0","id":7,"method":"prompts/list"}`,
	`{"jsonrpc":"2.0","id":8,"method":"prompts/get","params":{"name":"p"}}`,
	`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{"a":"<b>&"}}}`,
	`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"fail"}}`,
	`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"boom"}}`,
	`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"ctl","arguments":{"x":1}}}`,
	`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":99}}`,
	`{"jsonrpc":"2.0","id":13,"method":"nope/nothing"}`,
	`[1]`,
	`{"jsonrpc":"1.0","id":14,"method":"ping"}`,
	`{"jsonrpc":"2.0","id":15,"method":"ping"}`,
}

func goldenOptions() Options {
	o := testOptions()
	o.Tools = append(o.Tools, Tool{
		Name: "ctl", Control: true,
		Handler: func(_ context.Context, args json.RawMessage) (Result, error) {
			return Result{Text: "ctl:" + string(args)}, nil
		},
	})
	return o
}

// TestStdioTranscriptIsUnchanged pins the bytes Serve writes for a
// representative session.
func TestStdioTranscriptIsUnchanged(t *testing.T) {
	c := newClient(t, goldenOptions())
	var got bytes.Buffer
	for _, line := range goldenScript {
		var env struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal([]byte(line), &env)
		c.send(line)
		// A notification has no answer; the invalid ones answer anyway.
		if len(env.ID) == 0 && line[0] == '{' && !bytes.Contains([]byte(line), []byte(`"jsonrpc":"1.0"`)) {
			continue
		}
		got.Write(c.recvRaw())
	}
	const path = "testdata/stdio_transcript.golden"
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("stdio transcript changed:\n got: %s\nwant: %s", got.Bytes(), want)
	}
}
