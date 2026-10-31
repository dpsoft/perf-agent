package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ErrNotFound indicates every configured debuginfod URL returned 404 for
// the requested build-id.
var ErrNotFound = errors.New("debuginfod: not found")

type fetcher struct {
	client *http.Client
	urls   []string // pre-trimmed of trailing "/"
}

func newFetcher(urls []string, client *http.Client) *fetcher {
	if client == nil {
		client = http.DefaultClient
	}
	trimmed := make([]string, 0, len(urls))
	for _, u := range urls {
		trimmed = append(trimmed, strings.TrimRight(u, "/"))
	}
	return &fetcher{client: client, urls: trimmed}
}

// fetch tries each URL in order. Returns the response body on the first 200.
// 404 falls through to the next URL; non-200/404 records lastErr and
// continues. Caller is responsible for Close()ing the returned body.
func (f *fetcher) fetch(ctx context.Context, kind, buildID string) (io.ReadCloser, error) {
	body, _, err := f.fetchSized(ctx, kind, buildID)
	return body, err
}

// fetchSized is fetch, also returning the size of the body as read, or -1
// when the server did not say (see responseSize).
func (f *fetcher) fetchSized(ctx context.Context, kind, buildID string) (io.ReadCloser, int64, error) {
	var lastErr error
	for _, base := range f.urls {
		url := base + "/buildid/" + buildID + "/" + kind
		body, size, err := f.fetchURL(ctx, url)
		if err == nil {
			return body, size, nil
		}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		lastErr = err
	}
	if lastErr == nil {
		return nil, -1, ErrNotFound
	}
	return nil, -1, lastErr
}

// fetchURL returns the body on 200, ErrNotFound on 404, or a wrapped
// error on other statuses or transport failure.
func (f *fetcher) fetchURL(ctx context.Context, url string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, -1, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, -1, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, responseSize(resp), nil
	case http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, -1, ErrNotFound
	default:
		_ = resp.Body.Close()
		return nil, -1, fmt.Errorf("debuginfod: %s returned %d", url, resp.StatusCode)
	}
}

// responseSize is the size of resp's body as the caller reads it, or -1.
//
// Content-Length alone is not enough: Go asks for gzip on its own and
// decompresses transparently, and then reports ContentLength -1. Fedora's
// debuginfod does gzip on request, so every download from it looked unsized
// and could not be stopped early (issue #224, found on a live capture).
// debuginfod servers send the uncompressed size in X-Debuginfod-Size, which
// is the count the reader sees either way.
func responseSize(resp *http.Response) int64 {
	if resp.ContentLength >= 0 {
		return resp.ContentLength
	}
	if n, err := strconv.ParseInt(resp.Header.Get("X-Debuginfod-Size"), 10, 64); err == nil && n > 0 {
		return n
	}
	return -1
}
