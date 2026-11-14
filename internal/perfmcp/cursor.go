package perfmcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// cursorPrefix starts every cursor; the number is the cursor format.
const cursorPrefix = "c1."

// maxCursorBytes bounds the cursors decodeCursor reads.
const maxCursorBytes = 128

// binding identifies a query: the first 16 hex digits of the SHA-256 of the
// tool name, a NUL and the JSON of query, which holds every argument except
// limit and cursor. A cursor carries it so it cannot page another query.
func binding(tool string, query any) string {
	b, err := json.Marshal(query)
	if err != nil {
		// Queries are plain structs of strings and numbers.
		panic(fmt.Sprintf("perfmcp: binding of %s: %v", tool, err))
	}
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// encodeCursor is "c1.<offset>[,<offset>...].<binding>".
func encodeCursor(b string, offsets ...int) string {
	parts := make([]string, len(offsets))
	for i, o := range offsets {
		parts[i] = strconv.Itoa(o)
	}
	return cursorPrefix + strings.Join(parts, ",") + "." + b
}

// decodeCursor returns the offsets of cursor c, which must carry binding b
// and offsets no greater than n, written as encodeCursor writes them.
func decodeCursor(c, b string, n int) ([]int, error) {
	bad := func(why string) error {
		return fmt.Errorf("cursor %q is not valid here (%s); repeat the query without cursor to start from the first page", c, why)
	}
	if len(c) > maxCursorBytes {
		return nil, bad("too long")
	}
	rest, ok := strings.CutPrefix(c, cursorPrefix)
	if !ok {
		return nil, bad("not a cursor this server issued")
	}
	offs, got, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(got, ".") {
		return nil, bad("malformed")
	}
	if got != b {
		return nil, bad("it belongs to another query: the tool, handle, view or arguments differ")
	}
	var out []int
	for o := range strings.SplitSeq(offs, ",") {
		v, err := parseOffset(o)
		if err != nil {
			return nil, bad(err.Error())
		}
		if v > n {
			return nil, bad(fmt.Sprintf("offset %d is beyond the %d rows", v, n))
		}
		out = append(out, v)
	}
	return out, nil
}

// parseOffset reads a non-negative decimal with no sign and no leading zero.
func parseOffset(s string) (int, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" || (len(s) > 1 && s[0] == '0') {
		return 0, fmt.Errorf("offset %q is not a decimal", s)
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("offset %q is out of range", s)
	}
	return v, nil
}

// pageOut is one page of rows and what it leaves out after it.
type pageOut[T any] struct {
	Rows []T
	// OmittedCount and OmittedWeight cover the rows after this page:
	// their number and the sum of their own weights.
	OmittedCount  int
	OmittedWeight int64
	// Next is the offset of the next page, -1 on the last one.
	Next int
}

var errWeightOverflow = errors.New("the omitted rows' weights overflow a 64-bit sum")

// page returns rows[offset:offset+limit] and the count and weight sum of the
// rows after it. offset may equal len(rows) (an empty last page).
func page[T any](rows []T, weight func(T) int64, offset, limit int) (pageOut[T], error) {
	if limit <= 0 {
		return pageOut[T]{}, fmt.Errorf("limit must be positive, got %d", limit)
	}
	if offset < 0 || offset > len(rows) {
		return pageOut[T]{}, fmt.Errorf("offset %d is outside the %d rows", offset, len(rows))
	}
	end := offset + min(limit, len(rows)-offset)
	out := pageOut[T]{Rows: nonNil(rows[offset:end:end]), OmittedCount: len(rows) - end, Next: -1}
	for _, r := range rows[end:] {
		w := weight(r)
		if (w > 0 && out.OmittedWeight > math.MaxInt64-w) || (w < 0 && out.OmittedWeight < math.MinInt64-w) {
			return pageOut[T]{}, errWeightOverflow
		}
		out.OmittedWeight += w
	}
	if end < len(rows) {
		out.Next = end
	}
	return out, nil
}
