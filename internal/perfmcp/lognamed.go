package perfmcp

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// namingHandler names the binary the server runs as in place of
// capture.CommandMark before next handles a record: in its message, and
// in each string, error, fmt.Stringer and []error it carries, groups
// included. Any other value, a struct or a map holding the mark, is
// logged as it is. A record with no mark goes to next unchanged and
// uncopied. A nil pointer error or Stringer is left to next, which
// prints it as <nil>.
type namingHandler struct {
	next slog.Handler
	// command names the binary: it is the server's own command method,
	// so the log names what the server's answers name.
	command func() string
}

func (h namingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h namingHandler) Handle(ctx context.Context, r slog.Record) error {
	marked := strings.Contains(r.Message, capture.CommandMark)
	if !marked {
		r.Attrs(func(a slog.Attr) bool {
			_, marked = h.attr(a)
			return !marked
		})
	}
	if !marked {
		return h.next.Handle(ctx, r)
	}
	out := slog.NewRecord(r.Time, r.Level, h.text(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		named, _ := h.attr(a)
		out.AddAttrs(named)
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h namingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var named []slog.Attr
	for i, a := range attrs {
		n, changed := h.attr(a)
		if changed && named == nil {
			named = slices.Clone(attrs)
		}
		if named != nil {
			named[i] = n
		}
	}
	if named == nil {
		named = attrs
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

// attr is a with the mark replaced, and whether it changed: a string, the
// text of an error, a Stringer or a []error that carries the mark becomes
// a string attribute naming the binary, and a group is copied on its
// first attribute that changes. Anything else is a itself.
func (h namingHandler) attr(a slog.Attr) (slog.Attr, bool) {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		if s := v.String(); strings.Contains(s, capture.CommandMark) {
			return slog.String(a.Key, h.text(s)), true
		}
	case slog.KindGroup:
		group := v.Group()
		var out []slog.Attr
		for i, g := range group {
			n, changed := h.attr(g)
			if changed && out == nil {
				out = slices.Clone(group)
			}
			if out != nil {
				out[i] = n
			}
		}
		if out != nil {
			return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}, true
		}
	case slog.KindAny:
		if s, ok := h.anyText(v.Any()); ok {
			return slog.String(a.Key, s), true
		}
	}
	return a, false
}

// anyText is x's text with the mark replaced, when x is an error, a
// Stringer or a []error whose text carries the mark.
func (h namingHandler) anyText(x any) (string, bool) {
	var s string
	switch x := x.(type) {
	case error:
		if isNil(x) {
			return "", false
		}
		s = x.Error()
	case fmt.Stringer:
		if isNil(x) {
			return "", false
		}
		s = x.String()
	case []error:
		if !slices.ContainsFunc(x, func(e error) bool {
			return !isNil(e) && strings.Contains(e.Error(), capture.CommandMark)
		}) {
			return "", false
		}
		// fmt prints a nil element as <nil>, as the stock handlers do.
		s = fmt.Sprint(x)
	default:
		return "", false
	}
	if !strings.Contains(s, capture.CommandMark) {
		return "", false
	}
	return h.text(s), true
}

// isNil reports whether x is nil, or a nil pointer, map, slice, func,
// channel or interface held in an interface, whose methods may
// dereference it.
func isNil(x any) bool {
	if x == nil {
		return true
	}
	switch v := reflect.ValueOf(x); v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	}
	return false
}
