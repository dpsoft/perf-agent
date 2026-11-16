package mcphttp

import (
	"net/http"
	"time"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// smallBody is the largest body the ingress reserve takes. Every control
// message fits in it.
const smallBody = 64 << 10

// ingressReserve is what of the ingress bounds only bodies of at most
// smallBody may take: one reader, and smallBody bytes, as far as a body of
// MaxMessageBytes still fits beside it. The class of a body is not known
// until it is parsed, so the reserve goes by size: bodies over smallBody
// that stall cannot keep a cancel or a ping from being read.
func ingressReserve(o Options) (readers int, bytes int64) {
	if o.MaxIngressReaders > 1 {
		readers = 1
	}
	return readers, min(smallBody, max(o.MaxIngressBytes-o.MaxMessageBytes, 0))
}

// reserveIngress takes a reader and the bytes a body of contentLength
// announces, before any of it is read, or returns the refusal text naming
// the cap that is full. A body of unknown length reserves MaxMessageBytes,
// the most it may be. A body over smallBody leaves the reserve free. The
// returned function gives both back; the caller calls it as soon as the
// body is parsed or refused.
func (t *Transport) reserveIngress(contentLength int64) (func(), string) {
	n := contentLength
	if n < 0 {
		n = t.o.MaxMessageBytes
	}
	readers, bytes := t.o.MaxIngressReaders, t.o.MaxIngressBytes
	if n > smallBody {
		readers -= t.reserveReaders
		bytes -= t.reserveBytes
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.readers >= readers:
		return nil, busyText("MaxIngressReaders", int64(t.o.MaxIngressReaders))
	case t.reserved+n > bytes:
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
