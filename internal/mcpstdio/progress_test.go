package mcpstdio

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"testing"
	"time"
)

// slowWriter takes d per write, like a client that reads slowly but reads.
type slowWriter struct {
	w io.Writer
	d time.Duration
}

func (w slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.d)
	return w.w.Write(p)
}

func TestAProgressBurstIntoASlowClientIsDropped(t *testing.T) {
	const burst = 200
	o := withTools(Tool{Name: "chatty", Handler: func(ctx context.Context, _ json.RawMessage) (Result, error) {
		sent := 0
		for i := range burst {
			if Progress(ctx, float64(i), burst, "") {
				sent++
			}
		}
		return Result{Text: strconv.Itoa(sent)}, nil
	}})
	closes := newCloseLog(&o)
	sr, cw := io.Pipe()
	defer func() { _ = cw.Close() }()
	cr, sw := io.Pipe()
	defer func() { _ = cr.Close() }()
	go func() {
		_ = New(o).Serve(t.Context(), sr, slowWriter{sw, 2 * time.Millisecond})
		_ = sw.Close()
	}()
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"chatty","_meta":{"progressToken":"p"}}}` + "\n"
	if _, err := io.WriteString(cw, in); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(cr)
	notes := 0
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read: %v (OnClose %v)", err, closes.reasons())
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err)
		}
		if m.Method == "notifications/progress" {
			notes++
			continue
		}
		if string(m.ID) != "2" {
			continue
		}
		var r response
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		text, isErr := toolText(t, r)
		if isErr || text != strconv.Itoa(notes) {
			t.Fatalf("Progress reported %s sent, %d arrived (isError=%v)", text, notes, isErr)
		}
		if notes == burst {
			t.Fatalf("all %d notifications were queued for a slow client", burst)
		}
		break
	}
	if got := closes.reasons(); len(got) != 0 {
		t.Fatalf("OnClose = %v", got)
	}
}

// Progress through a context that outlives the call sends nothing once the
// call is answered.
func TestNoProgressAfterTheAnswer(t *testing.T) {
	leaked := make(chan context.Context, 1)
	o := withTools(Tool{Name: "leak", Handler: func(ctx context.Context, _ json.RawMessage) (Result, error) {
		leaked <- context.WithoutCancel(ctx)
		return Result{Text: "done"}, nil
	}})
	c := newClient(t, o)
	c.init()
	wantID(t, c.call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"leak","_meta":{"progressToken":"p"}}}`), "2")
	ctx := <-leaked
	if Progress(ctx, 1, 1, "late") {
		t.Fatal("Progress after the answer returned true")
	}
	r := c.call(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`, 3))
	wantID(t, r, "3")
}
