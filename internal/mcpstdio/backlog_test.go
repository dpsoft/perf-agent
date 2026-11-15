package mcpstdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingWriter holds every Write until the gate opens, then keeps each
// line it is given.
type recordingWriter struct {
	entered chan struct{}
	gate    chan struct{}
	lines   chan []byte
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{entered: make(chan struct{}, 64), gate: make(chan struct{}), lines: make(chan []byte, 64)}
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.entered <- struct{}{}
	<-w.gate
	w.lines <- bytes.Clone(p)
	return len(p), nil
}

// Six pipelined answers of 2 MiB each (1 MiB of quotes, escaped), with
// MaxOutBytes 4 MiB, to a live client reading a real pipe 16 KiB a
// millisecond.
func TestALiveClientWithBigPipelinedAnswersStaysAlive(t *testing.T) {
	const calls, size = 6, 1 << 20
	quotes := strings.Repeat(`"`, size)
	o := withTools(Tool{Name: "big", Handler: func(context.Context, json.RawMessage) (Result, error) {
		return Result{Text: quotes}, nil
	}})
	o.MaxToolErrorBytes = size
	closes := newCloseLog(&o)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close() }()
	sr, cw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := New(o).Serve(t.Context(), sr, pw)
		_ = pw.Close()
		done <- err
	}()
	got := make(chan int, 1)
	go func() {
		// A steady client: 16 KiB, then a millisecond's pause.
		buf := make([]byte, 16<<10)
		n := 0
		for {
			k, err := pr.Read(buf)
			n += bytes.Count(buf[:k], []byte("\n"))
			if err != nil {
				got <- n
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	var in strings.Builder
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n")
	for i := range calls {
		in.WriteString(callLine(2+i, "big", "{}") + "\n")
	}
	if _, err := io.WriteString(cw, in.String()); err != nil {
		t.Fatal(err)
	}
	_ = cw.Close()
	if err := <-done; err != nil {
		t.Fatalf("Serve = %v (OnClose %v)", err, closes.reasons())
	}
	if n := <-got; n != calls+1 {
		t.Fatalf("%d lines, want %d", n, calls+1)
	}
	if got := closes.reasons(); !slices.Equal(got, []CloseReason{CloseEOF}) {
		t.Fatalf("OnClose = %v, want [CloseEOF]", got)
	}
}

// A 5 MiB line (over the 4 MiB bound) behind a small line still queued
// while another is being written.
func TestABigLineBehindAQueuedOneFits(t *testing.T) {
	w := newRecordingWriter()
	o := newOutQueue(w, 64, 4<<20, time.Minute)
	go o.run()
	defer o.shutdown()
	lines := [][]byte{[]byte("a\n"), []byte("b\n"), append(bytes.Repeat([]byte("c"), 5<<20), '\n')}
	if !o.enqueue(lines[0]) {
		t.Fatal("the first line was refused")
	}
	<-w.entered // a is being written
	for _, l := range lines[1:] {
		if !o.enqueue(l) {
			t.Fatalf("a %d-byte line was refused behind %d queued", len(l), 1)
		}
	}
	close(w.gate)
	for _, want := range lines {
		if got := <-w.lines; !bytes.Equal(got, want) {
			t.Fatalf("wrote %.20q, want %.20q", got, want)
		}
	}
	select {
	case <-o.dead:
		t.Fatal("the client was declared dead")
	default:
	}
}

func TestAReaderThatStopsIsDeadWithinStallTimeout(t *testing.T) {
	o := testOptions()
	o.StallTimeout = 200 * time.Millisecond
	closes := newCloseLog(&o)
	sr, cw := io.Pipe()
	defer func() { _ = cw.Close() }()
	w := newBlockedWriter(t)
	done := make(chan error, 1)
	go func() { done <- New(o).Serve(t.Context(), sr, w) }()
	if _, err := io.WriteString(cw, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	<-w.entered
	start := time.Now()
	if _, err := io.WriteString(cw, `{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		took := time.Since(start)
		if !errors.Is(err, ErrClientGone) {
			t.Fatalf("Serve = %v, want ErrClientGone", err)
		}
		if took < o.StallTimeout {
			t.Errorf("declared dead after %v, before StallTimeout", took)
		}
	case <-time.After(o.StallTimeout + time.Second):
		t.Fatal("a client that stopped reading was not declared dead")
	}
	if got := closes.reasons(); !slices.Equal(got, []CloseReason{CloseClientGone}) {
		t.Fatalf("OnClose = %v, want [CloseClientGone]", got)
	}
}

// A client that keeps 1+MaxQueued requests outstanding is never told the
// server is busy.
func TestOneRunningAndMaxQueuedQueuedAreAccepted(t *testing.T) {
	c := newClient(t, withTools(controlTool()))
	c.init()
	const n, window = 2000, 9
	slots := make(chan struct{}, window)
	var mu sync.Mutex
	var writeErr error
	go func() {
		for id := 2; id < n+2; id++ {
			slots <- struct{}{}
			line := callLine(id, "echo", "{}") + "\n"
			if id%7 == 0 {
				line += callLine(1_000_000+id, "ctl", "{}") + "\n"
			}
			if _, err := io.WriteString(c.w, line); err != nil {
				mu.Lock()
				writeErr = err
				mu.Unlock()
				return
			}
		}
	}()
	br := bufio.NewReader(c.r)
	busy, last := 0, 1
	for seen := 0; seen < n; {
		line, err := br.ReadBytes('\n')
		if err != nil {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("read: %v (write: %v)", err, writeErr)
		}
		var r response
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		var id int
		if _, err := fmt.Sscan(string(r.ID), &id); err != nil {
			t.Fatal(err)
		}
		if id >= 1_000_000 {
			continue
		}
		seen++
		<-slots
		if r.Error != nil && r.Error.Code == codeServerBusy {
			busy++
			continue
		}
		if id <= last {
			t.Fatalf("id %d after %d", id, last)
		}
		last = id
	}
	if busy != 0 {
		t.Fatalf("%d busy answers with %d outstanding", busy, window)
	}
}
