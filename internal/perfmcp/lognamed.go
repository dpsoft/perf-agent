package perfmcp

import (
	"context"
	"log/slog"
	"strings"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// namingHandler names the binary the server runs as in place of
// capture.CommandMark, in a record's message and in each string or error
// it carries, before next handles it: a capture.Error logged as it is
// reads as the agent reads it. command is read at each record, since a
// server's name is set once New has built it.
type namingHandler struct {
	next    slog.Handler
	command func() string
}

func (h namingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h namingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, h.text(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h namingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	named := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		named[i] = h.attr(a)
	}
	return namingHandler{next: h.next.WithAttrs(named), command: h.command}
}

func (h namingHandler) WithGroup(name string) slog.Handler {
	return namingHandler{next: h.next.WithGroup(name), command: h.command}
}

// text is s with the mark replaced, and s itself when it has none.
func (h namingHandler) text(s string) string {
	if !strings.Contains(s, capture.CommandMark) {
		return s
	}
	return named(s, h.command())
}

// attr is a with the mark replaced in a string, in an error's text, and
// in each attribute of a group; anything else is left as it is.
func (h namingHandler) attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		if s := v.String(); strings.Contains(s, capture.CommandMark) {
			return slog.String(a.Key, h.text(s))
		}
	case slog.KindGroup:
		group := v.Group()
		named := make([]slog.Attr, len(group))
		for i, g := range group {
			named[i] = h.attr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(named...)}
	case slog.KindAny:
		if err, ok := v.Any().(error); ok && err != nil {
			if s := err.Error(); strings.Contains(s, capture.CommandMark) {
				return slog.String(a.Key, h.text(s))
			}
		}
	}
	return a
}
