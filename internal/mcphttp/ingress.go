package mcphttp

import (
	"net/http"
	"time"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// reserveIngress takes a reader and the bytes a body of contentLength
// announces, before any of it is read, or returns the refusal text naming
// the cap that is full. A body of unknown length reserves MaxMessageBytes,
// the most it may be. The returned function gives both back; the caller
// calls it as soon as the body is parsed or refused.
func (t *Transport) reserveIngress(contentLength int64) (func(), string) {
	n := contentLength
	if n < 0 {
		n = t.o.MaxMessageBytes
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.readers >= t.o.MaxIngressReaders:
		return nil, busyText("MaxIngressReaders", int64(t.o.MaxIngressReaders))
	case t.reserved+n > t.o.MaxIngressBytes:
		return nil, busyText("MaxIngressBytes", t.o.MaxIngressBytes)
	}
	t.readers++
	t.reserved += n
	return func() {
		t.mu.Lock()
		t.readers--
		t.reserved -= n
		t.mu.Unlock()
	}, ""
}

// parseBody reads r's body and parses it, and stamps the receipt time once
// the body is in. The raw body is dropped on return: a request keeps only
// what Parse copied out of it, so a call's retained bytes are its params.
func (t *Transport) parseBody(w http.ResponseWriter, rc *http.ResponseController, r *http.Request) (*mcpstdio.Request, []byte, time.Time, error) {
	msg, err := t.readBody(w, rc, r)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	received := time.Now()
	req, bad := t.h.Parse(msg)
	return req, bad, received, nil
}
