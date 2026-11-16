package perfmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// A free slot is taken whatever the context says, so a call that does not
// wait is never refused; only a call that would wait sees the context.
func TestAcquireTakesAFreeSlotEvenWithACancelledContext(t *testing.T) {
	e := newEngine()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for i := range 1000 {
		if err := e.acquire(ctx); err != nil {
			t.Fatalf("try %d: a free slot with a cancelled context: %v", i, err)
		}
		if err := e.acquire(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("try %d: a held slot with a cancelled context: %v, want context.Canceled", i, err)
		}
		e.release()
	}

	// A wait ends when the holder releases.
	if err := e.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() { got <- e.acquire(t.Context()) }()
	select {
	case err := <-got:
		t.Fatalf("acquired a held slot: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	e.release()
	if err := <-got; err != nil {
		t.Fatalf("after release: %v", err)
	}
	e.release()
}

// writeCPUFixtures writes cpu.pb.gz and cpu-new.pb.gz into cfg's profiles root.
func writeCPUFixtures(t *testing.T, cfg Config) {
	t.Helper()
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	writeFixture(t, cfg.ProfilesDir, "cpu-new.pb.gz", pmfixture.CPUBasicNew())
}

type openedAs struct {
	Handle      profilemodel.Handle `json:"handle"`
	DefaultView string              `json:"default_view"`
}

// A tool waits while something else, such as a capture job, holds the
// engine, and answers once it is released.
func TestTheEngineSerialisesToolsWithAJob(t *testing.T) {
	cfg := testConfig(t)
	writeCPUFixtures(t, cfg)
	c := newTestServer(t, cfg)
	var o openedAs
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)

	if err := c.srv.engine.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"top","arguments":{"handle":%q,"view":%q,"rows":"functions"}}}`+"\n", o.Handle, o.DefaultView)
	if _, err := c.w.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	line := make(chan []byte, 1)
	go func() {
		b, _ := c.r.ReadBytes('\n')
		line <- b
	}()
	select {
	case b := <-line:
		c.srv.engine.release()
		t.Fatalf("top answered while the engine was held: %s", b)
	case <-time.After(200 * time.Millisecond):
	}
	c.srv.engine.release()
	var res struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	b := <-line
	if err := json.Unmarshal(b, &res); err != nil || res.Result.IsError {
		t.Fatalf("top after release: %s (%v)", b, err)
	}
}

// Calls pipelined behind EOF run with a cancelled context, and the engine
// is free for each in turn, so all are answered.
func TestPipelinedCallsAtEOFAreAnswered(t *testing.T) {
	cfg := testConfig(t)
	writeCPUFixtures(t, cfg)
	var o openedAs
	newTestServer(t, cfg).callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)

	s := must(New(cfg))(t)
	defer func() { _ = s.Close() }()
	var in strings.Builder
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n")
	for i, call := range []string{
		`{"name":"open","arguments":{"path":"cpu.pb.gz"}}`,
		fmt.Sprintf(`{"name":"top","arguments":{"handle":%q,"view":%q,"rows":"nodes"}}`, o.Handle, o.DefaultView),
		fmt.Sprintf(`{"name":"diff","arguments":{"base_handle":%q,"base_view":%q,"new_handle":%q,"new_view":%q}}`, o.Handle, o.DefaultView, o.Handle, o.DefaultView),
	} {
		fmt.Fprintf(&in, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":%s}`+"\n", 2+i, call)
	}
	var out bytes.Buffer
	if err := s.Serve(t.Context(), strings.NewReader(in.String()), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	sc := bufio.NewScanner(&out)
	sc.Buffer(nil, 4<<20)
	n := 0
	for sc.Scan() {
		var res struct {
			ID     int `json:"id"`
			Result struct {
				IsError bool                    `json:"isError"`
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(sc.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Result.IsError {
			t.Errorf("id %d: isError: %v", res.ID, res.Result.Content)
		}
		n++
	}
	if n != 4 {
		t.Fatalf("%d answers, want 4:\n%s", n, out.String())
	}
}

// The page_* tools change the store, the pins and the live state, as a
// capture job does: they hold the engine, so the two never run at once.
func TestLivePageToolsHoldTheEngine(t *testing.T) {
	cfg := testConfig(t)
	cfg.Page = livePage()
	writeCPUFixtures(t, cfg)
	c := newTestServer(t, cfg)
	var o openedAs
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)
	c.callOK("page_open", map[string]any{}, nil)

	s := c.srv
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := s.engine.acquire(t.Context()); err != nil {
				return
			}
			for _, name := range []string{"cpu.pb.gz", "cpu-new.pb.gz"} {
				if _, err := s.store.open(name, ""); err != nil {
					t.Error(err)
				}
			}
			s.engine.release()
		}
	})
	for range 20 {
		var pub struct {
			Publication string `json:"publication"`
		}
		c.callOK("page_explain", map[string]any{"handle": o.Handle, "view": o.DefaultView, "explanations": []any{}}, &pub)
		c.callOK("page_show", map[string]any{"publication": pub.Publication}, nil)
	}
	close(stop)
	wg.Wait()
}

// open and diff answer exactly what openResultFor and diffPage return.
func TestOpenAndDiffResultsAreUnchanged(t *testing.T) {
	cfg := testConfig(t)
	writeCPUFixtures(t, cfg)
	c := newTestServer(t, cfg)
	s := c.srv
	var o openedAs
	text := c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)
	var nw openedAs
	c.callOK("open", map[string]any{"path": "cpu-new.pb.gz"}, &nw)

	if err := s.engine.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	direct, err := s.openResultFor("cpu.pb.gz", "")
	s.engine.release()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(must(marshal(direct))(t)); got != text {
		t.Errorf("openResultFor:\n%s\nopen:\n%s", got, text)
	}

	q := diffQuery{BaseHandle: o.Handle, BaseView: o.DefaultView, NewHandle: nw.Handle, NewView: nw.DefaultView}
	args := map[string]any{"base_handle": q.BaseHandle, "base_view": q.BaseView, "new_handle": q.NewHandle, "new_view": q.NewView, "limit": 2}
	var first diffResult
	text = c.callOK("diff", args, &first)
	cursor := first.NextCursor
	if cursor == "" {
		t.Fatal("the first page has no next_cursor")
	}
	args["cursor"] = cursor
	second := c.callOK("diff", args, nil)

	if err := s.engine.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.engine.release()
	for _, page := range []struct{ cursor, want string }{{"", text}, {cursor, second}} {
		d, err := s.diffPage(q, 2, page.cursor)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(must(marshal(d))(t)); got != page.want {
			t.Errorf("diffPage(cursor %q):\n%s\ndiff:\n%s", page.cursor, got, page.want)
		}
	}
}

// publishNew names the root it writes in, publishes with the mode it is
// given, and never replaces a name; writePage's texts are unchanged.
func TestPublishNewNamesItsRoot(t *testing.T) {
	cfg := testConfig(t)
	cfg.OutputDir = t.TempDir()
	s := must(New(cfg))(t)
	defer func() { _ = s.Close() }()
	dir := cfg.ProfilesDir

	name, err := s.publishNew(s.profiles, rootProfiles, ".", ".owner", []byte("stdio\n"), 0o600)
	if err != nil || name != ".owner" {
		t.Fatalf("publishNew = %q, %v", name, err)
	}
	fi, err := os.Stat(filepath.Join(dir, ".owner"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("published %v, %v; want mode 0600", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".owner")); string(b) != "stdio\n" {
		t.Errorf("published %q", b)
	}
	noTemps := func() {
		t.Helper()
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if strings.Contains(e.Name(), ".tmp-") {
				t.Errorf("temporary %s left behind", e.Name())
			}
		}
	}

	_, err = s.publishNew(s.profiles, rootProfiles, ".", ".owner", []byte("serve\n"), 0o600)
	if !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), "profiles root") {
		t.Errorf("a taken name: %v, want fs.ErrExist naming the profiles root", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".owner")); string(b) != "stdio\n" {
		t.Errorf("a taken name was replaced: %q", b)
	}
	noTemps()

	s.link = func(_ *os.Root, oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	_, err = s.publishNew(s.profiles, rootProfiles, ".", "intent", []byte("x"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "under the profiles root") || strings.Contains(err.Error(), dir) ||
		!strings.Contains(err.Error(), "--captures") || strings.Contains(err.Error(), "--output") {
		t.Errorf("a refused hard link: %v, want it to name the profiles root by its kind and --captures", err)
	}
	noTemps()
	s.link = (*os.Root).Link

	// The temporary's messages name the root and what is written.
	_, err = writeTemp(s.profiles, rootProfiles, "missing", "x", nil, 0o600)
	if want := fmt.Sprintf("cannot create a temporary file in missing under the profiles root %s: ", dir); err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("writeTemp(profiles) = %v, want it to start %q", err, want)
	}
	_, err = writeTemp(s.output, rootOutput, "missing", "x.html", nil, 0o644)
	if want := fmt.Sprintf("cannot create a temporary page in missing under the output root %s: ", cfg.OutputDir); err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("writeTemp(output) = %v, want it to start %q", err, want)
	}

	// writePage: a taken name is the same text as before, and still
	// fs.ErrExist underneath.
	writeFixture(t, cfg.OutputDir, "x.html", []byte("x"))
	if err := s.linkNoReplace(s.output, rootOutput, "x.html", "x.html"); !errors.Is(err, fs.ErrExist) ||
		err.Error() != existsError("x.html", cfg.OutputDir).Error() {
		t.Errorf("linkNoReplace(output) = %v", err)
	}
}

func TestOpenNamesFollowsTheStore(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxOpenHandles = 1
	writeCPUFixtures(t, cfg)
	c := newTestServer(t, cfg)
	if got := c.srv.store.openNames(); got == nil || len(got) != 0 {
		t.Fatalf("an empty store: %v", got)
	}
	var a, b openedAs
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &a)
	first := c.srv.store.openNames()
	if len(first) != 1 || first["cpu.pb.gz"] != a.Handle {
		t.Fatalf("after one open: %v", first)
	}
	c.callOK("open", map[string]any{"path": filepath.Join(cfg.ProfilesDir, "cpu-new.pb.gz")}, &b)
	second := c.srv.store.openNames()
	if len(second) != 1 || second["cpu-new.pb.gz"] != b.Handle {
		t.Fatalf("after an eviction: %v", second)
	}
	if len(first) != 1 || first["cpu.pb.gz"] != a.Handle {
		t.Errorf("a published snapshot changed: %v", first)
	}
}
