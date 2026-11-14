package explain

import (
	"slices"
	"testing"
)

func seg(kind SegmentKind, text string) Segment { return Segment{Kind: kind, Text: text} }

func TestTokenizeMarksFreeNumbers(t *testing.T) {
	cases := []struct {
		in   string
		want []Segment
	}{
		{
			"{c1} of time; 62% claimed; 1,416 runs; sm86_xmma v2 f9",
			[]Segment{
				{Kind: SegmentClaim, Text: "{c1}", Claim: "c1"},
				seg(SegmentText, " of time; "),
				seg(SegmentUnchecked, "62%"),
				seg(SegmentText, " claimed; "),
				seg(SegmentUnchecked, "1,416"),
				seg(SegmentText, " runs; sm86_xmma v2 f9"),
			},
		},
		{
			"{n:0123456789abcdef} and {x",
			[]Segment{
				{Kind: SegmentNode, Text: "{n:0123456789abcdef}", Node: "0123456789abcdef"},
				seg(SegmentText, " and {x"),
			},
		},
		{"operator{}", []Segment{seg(SegmentText, "operator{}")}},
		{"", nil},
	}
	for _, c := range cases {
		if got := Tokenize(c.in); !slices.Equal(got, c.want) {
			t.Errorf("Tokenize(%q)\n got %+v\nwant %+v", c.in, got, c.want)
		}
	}
}

func TestTokenizeNumberEdges(t *testing.T) {
	cases := []struct {
		in   string
		want []Segment
	}{
		// A trailing separator without a digit after it is text, and only
		// one % is taken.
		{"3.14. and 5,", []Segment{seg(SegmentUnchecked, "3.14"), seg(SegmentText, ". and "), seg(SegmentUnchecked, "5"), seg(SegmentText, ",")}},
		{"62%%", []Segment{seg(SegmentUnchecked, "62%"), seg(SegmentText, "%")}},
		// Preceded by a letter, digit, underscore or dot: not a number start.
		{"x.5 _7 é9", []Segment{seg(SegmentText, "x.5 _7 é9")}},
		// After a placeholder or punctuation a digit starts a number.
		{"{c1}5 (8)", []Segment{
			{Kind: SegmentClaim, Text: "{c1}", Claim: "c1"},
			seg(SegmentUnchecked, "5"), seg(SegmentText, " ("), seg(SegmentUnchecked, "8"), seg(SegmentText, ")"),
		}},
		// Digits in other scripts are numbers too, so they cannot pass as
		// prose.
		{"about ６２%", []Segment{seg(SegmentText, "about "), seg(SegmentUnchecked, "６２%")}},
		// Node references need exactly 16 lowercase hex digits.
		{"{n:0123456789ABCDEF}", []Segment{seg(SegmentText, "{n:"), seg(SegmentUnchecked, "0123456789"), seg(SegmentText, "ABCDEF}")}},
		{"{n:abcdefabcdefabcd}", []Segment{{Kind: SegmentNode, Text: "{n:abcdefabcdefabcd}", Node: "abcdefabcdefabcd"}}},
		{"{n:0123}", []Segment{seg(SegmentText, "{n:"), seg(SegmentUnchecked, "0123"), seg(SegmentText, "}")}},
		// A claim ID starts with a letter and has at most 32 characters.
		{"{1c}", []Segment{seg(SegmentText, "{"), seg(SegmentUnchecked, "1"), seg(SegmentText, "c}")}},
		{"{a234567890123456789012345678901bc}", []Segment{seg(SegmentText, "{a234567890123456789012345678901bc}")}},
		{"{a2345678901234567890123456789012}", []Segment{{Kind: SegmentClaim, Text: "{a2345678901234567890123456789012}", Claim: "a2345678901234567890123456789012"}}},
		{"x{c-1_b}y", []Segment{seg(SegmentText, "x"), {Kind: SegmentClaim, Text: "{c-1_b}", Claim: "c-1_b"}, seg(SegmentText, "y")}},
	}
	for _, c := range cases {
		if got := Tokenize(c.in); !slices.Equal(got, c.want) {
			t.Errorf("Tokenize(%q)\n got %+v\nwant %+v", c.in, got, c.want)
		}
	}
}

func TestTokenizeKeepsEveryByte(t *testing.T) {
	for _, s := range []string{"{c1} of 62% {n:0123456789abcdef}{", "\xff9 bad utf8 {c", "a1,2.3%{}"} {
		var joined string
		for _, sg := range Tokenize(s) {
			joined += sg.Text
		}
		if joined != s {
			t.Errorf("segments of %q join to %q", s, joined)
		}
	}
}
