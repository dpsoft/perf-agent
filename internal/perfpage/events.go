package perfpage

import (
	"time"

	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// The page's stream: a keep-alive comment after this long without an
// event, and this long for each write before the stream is dropped.
const (
	eventsKeepAlive    = 15 * time.Second
	eventsWriteTimeout = 10 * time.Second
)

// NewEvents is the events hub the page's stream is served from: at most
// lim.MaxPageClients streams at once. Its Publish is the perfmcp.PageConfig
// Notify, and its Clients the Viewers.
func NewEvents(lim perfmcp.Limits) *livehttp.Hub {
	return livehttp.NewHub(lim.MaxPageClients, eventsKeepAlive, eventsWriteTimeout)
}
