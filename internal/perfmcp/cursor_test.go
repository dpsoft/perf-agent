package perfmcp

import (
	"math"
	"slices"
	"strings"
	"testing"
)

type testQuery struct {
	Handle string `json:"handle"`
	View   string `json:"view"`
	By     string `json:"by"`
}

func TestCursorsAreBoundToTheirQuery(t *testing.T) {
	q := testQuery{Handle: "h", View: "cpu/nanoseconds#0", By: "cum"}
	b := binding("top", q)
	if len(b) != 16 || strings.Trim(b, "0123456789abcdef") != "" {
		t.Fatalf("binding %q is not 16 hex", b)
	}
	if b != binding("top", q) {
		t.Error("binding is not deterministic")
	}
	other := binding("top", testQuery{Handle: "h", View: "cpu/nanoseconds#0", By: "self"})
	if other == b || binding("sandwich", q) == b {
		t.Error("bindings of different queries collide")
	}

	c := encodeCursor(b, 20)
	if c != "c1.20."+b {
		t.Errorf("cursor = %q", c)
	}
	if got, err := decodeCursor(c, b, 30, 1); err != nil || !slices.Equal(got, []int{20}) {
		t.Errorf("decode = %v, %v", got, err)
	}
	two := encodeCursor(b, 3, 0)
	if got, err := decodeCursor(two, b, 5, 2); err != nil || !slices.Equal(got, []int{3, 0}) {
		t.Errorf("decode two = %v, %v", got, err)
	}

	if _, err := decodeCursor(c, other, 30, 1); err == nil || !strings.Contains(err.Error(), "another query") {
		t.Errorf("another binding: %v", err)
	}
	if _, err := decodeCursor(c, b, 19, 1); err == nil {
		t.Error("offset beyond n accepted")
	}
	for _, bad := range []string{
		"", "garbage", "c1.", "c1.20", "c2.20." + b, "c1.-1." + b, "c1.+5." + b, "c1.020." + b,
		"c1.x." + b, "c1.." + b, "c1.1,." + b, "c1.20." + b + ".x", "c1.99999999999999999999999." + b,
	} {
		if _, err := decodeCursor(bad, b, 30, 1); err == nil {
			t.Errorf("decodeCursor(%q) accepted", bad)
		}
	}
	// An edited cursor with another number of offsets is refused, so a tool
	// paging two lists never indexes an offset that is not there.
	if _, err := decodeCursor(c, b, 30, 2); err == nil || !strings.Contains(err.Error(), "without cursor") {
		t.Errorf("one offset where two are paged: %v", err)
	}
	if _, err := decodeCursor(two, b, 5, 1); err == nil {
		t.Error("two offsets where one is paged were accepted")
	}
	if _, err := decodeCursor(encodeCursor(b, 3, 6), b, 5, 2); err == nil {
		t.Error("a second offset beyond n was accepted")
	}
}

func TestPageCountsAndWeighsTheOmittedRows(t *testing.T) {
	rows := []int64{50, 40, 30, 20, 10}
	w := func(x int64) int64 { return x }

	p := must(page(rows, w, 0, 2))(t)
	if !slices.Equal(p.Rows, []int64{50, 40}) || p.OmittedCount != 3 || p.OmittedWeight != 60 || p.Next != 2 {
		t.Errorf("first page = %+v", p)
	}
	p = must(page(rows, w, 2, 2))(t)
	if !slices.Equal(p.Rows, []int64{30, 20}) || p.OmittedCount != 1 || p.OmittedWeight != 10 || p.Next != 4 {
		t.Errorf("second page = %+v", p)
	}
	p = must(page(rows, w, 4, 2))(t)
	if !slices.Equal(p.Rows, []int64{10}) || p.OmittedCount != 0 || p.OmittedWeight != 0 || p.Next != -1 {
		t.Errorf("last page = %+v", p)
	}
	p = must(page([]int64(nil), w, 0, 2))(t)
	if len(p.Rows) != 0 || p.Rows == nil || p.Next != -1 {
		t.Errorf("empty page = %+v", p)
	}

	if _, err := page(rows, w, 6, 2); err == nil {
		t.Error("offset beyond the rows accepted")
	}
	if _, err := page(rows, w, -1, 2); err == nil {
		t.Error("negative offset accepted")
	}
	if _, err := page(rows, w, 0, 0); err == nil {
		t.Error("zero limit accepted")
	}
	if _, err := page([]int64{1, math.MaxInt64, 1}, w, 0, 1); err == nil {
		t.Error("an overflowing omitted weight was accepted")
	}
}
