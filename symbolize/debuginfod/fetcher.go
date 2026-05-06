package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	var lastErr error
	for _, base := range f.urls {
		url := base + "/buildid/" + buildID + "/" + kind
		body, err := f.fetchURL(ctx, url)
		if err == nil {
			return body, nil
		}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		lastErr = err
	}
	if lastErr == nil {
		return nil, ErrNotFound
	}
	return nil, lastErr
}

// fetchURL returns the body on 200, ErrNotFound on 404, or a wrapped
// error on other statuses or transport failure.
func (f *fetcher) fetchURL(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, ErrNotFound
	default:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("debuginfod: %s returned %d", url, resp.StatusCode)
	}
}
