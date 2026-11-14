package explain

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// maxClaimIDLen is the longest claim ID: a letter and up to 31 more.
const maxClaimIDLen = 32

// nodeRefPrefix starts a node placeholder.
const nodeRefPrefix = "{n:"

// nodeIDLen is the length of a NodeID: 16 lowercase hex digits.
const nodeIDLen = 16

// maxPlaceholderLen bounds the search for a placeholder's "}", so text full
// of "{" is tokenized in linear time.
const maxPlaceholderLen = max(len("{}")+maxClaimIDLen, len("{n:}")+nodeIDLen)

// validClaimID reports whether id matches ^[A-Za-z][A-Za-z0-9_-]{0,31}$.
func validClaimID(id string) bool {
	if id == "" || len(id) > maxClaimIDLen || !isASCIILetter(id[0]) {
		return false
	}
	for i := 1; i < len(id); i++ {
		c := id[i]
		if !isASCIILetter(c) && !isASCIIDigit(c) && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func isASCIILetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

func isASCIIDigit(c byte) bool { return '0' <= c && c <= '9' }

func isLowerHex(c byte) bool { return isASCIIDigit(c) || 'a' <= c && c <= 'f' }

// Tokenize splits agent text into segments. "{<claim id>}" is a claim
// placeholder and "{n:<16 lowercase hex>}" a node placeholder; any other "{"
// is text. A number starts at a numeric character (unicode.IsNumber: any
// script's digits, and "½", "²", "①") that does not follow an ASCII letter,
// a numeric character or "_", or at a "." before one, unless that "." itself
// follows an ASCII letter or numeric character. It continues over numeric
// characters and over a "." or "," followed by one, and takes one trailing
// "%"; it becomes an unchecked_number segment, so a figure no claim checked
// is never shown as if it were one. Only ASCII letters hide a number, as in
// "sm86", "x_1" and "v1.2", and never one ending in "%" ("about41%" is a
// figure): prose in a script without spaces ("占用了41.1%") or after a
// filler character is still marked. Everything else is text,
// with adjacent text merged.
//
// Only agent text is tokenized. Frame names are never passed here: a name
// like "operator{}" or one containing "{c1}" is a name, not a placeholder.
func Tokenize(s string) []Segment {
	var out []Segment
	textStart := 0
	flush := func(end int) {
		if end > textStart {
			out = append(out, Segment{Kind: SegmentText, Text: s[textStart:end]})
		}
	}
	for i := 0; i < len(s); {
		if s[i] == '{' {
			if sg, n, ok := placeholder(s[i:]); ok {
				flush(i)
				out = append(out, sg)
				i += n
				textStart = i
				continue
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		n := 0
		switch {
		case unicode.IsNumber(r) && (numberMayStart(s[:i]) || endsInPercent(s[i:])):
			n = numberLen(s[i:])
		case r == '.' && digitsLen(s[i+1:]) > 0 && dotMayStart(s[:i]):
			n = 1 + numberLen(s[i+1:])
		}
		if n > 0 {
			flush(i)
			out = append(out, Segment{Kind: SegmentUnchecked, Text: s[i : i+n]})
			i += n
			textStart = i
			continue
		}
		i += size
	}
	flush(len(s))
	return out
}

// placeholder parses a placeholder at the start of s, which starts with
// "{", returning the segment and its length in bytes.
func placeholder(s string) (Segment, int, bool) {
	end := strings.IndexByte(s[:min(len(s), maxPlaceholderLen)], '}')
	if end < 0 {
		return Segment{}, 0, false
	}
	inner := s[1:end]
	if id, ok := strings.CutPrefix(inner, nodeRefPrefix[1:]); ok {
		if len(id) != nodeIDLen {
			return Segment{}, 0, false
		}
		for i := range len(id) {
			if !isLowerHex(id[i]) {
				return Segment{}, 0, false
			}
		}
		return Segment{Kind: SegmentNode, Text: s[:end+1], Node: profilemodel.NodeID(id)}, end + 1, true
	}
	if !validClaimID(inner) {
		return Segment{}, 0, false
	}
	return Segment{Kind: SegmentClaim, Text: s[:end+1], Claim: inner}, end + 1, true
}

// numberMayStart reports whether a number may start at a numeric character
// right after before: not after an ASCII letter, a numeric character or
// "_", and not after a "." that could not start one either.
func numberMayStart(before string) bool {
	r, size := utf8.DecodeLastRuneInString(before)
	switch {
	case before == "":
		return true
	case r == '.':
		return dotMayStart(before[:len(before)-size])
	}
	return !isASCIILetterRune(r) && r != '_' && !unicode.IsNumber(r)
}

func isASCIILetterRune(r rune) bool { return r < utf8.RuneSelf && isASCIILetter(byte(r)) }

// dotMayStart reports whether a "." right after before may start a number:
// not after an ASCII letter or a numeric character, as in "x.5" or "1.2".
func dotMayStart(before string) bool {
	r, _ := utf8.DecodeLastRuneInString(before)
	return before == "" || !isASCIILetterRune(r) && !unicode.IsNumber(r)
}

// endsInPercent reports whether the number at the start of s, which starts
// with a numeric character, ends in "%".
func endsInPercent(s string) bool {
	n := numberLen(s)
	return s[n-1] == '%'
}

// numberLen is the length in bytes of the number at the start of s, which
// starts with a numeric character.
func numberLen(s string) int {
	i := digitsLen(s)
	for i < len(s) && (s[i] == '.' || s[i] == ',') {
		n := digitsLen(s[i+1:])
		if n == 0 {
			break
		}
		i += 1 + n
	}
	if i < len(s) && s[i] == '%' {
		i++
	}
	return i
}

// digitsLen is the length in bytes of the run of numeric characters at the
// start of s.
func digitsLen(s string) int {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsNumber(r) {
			break
		}
		i += size
	}
	return i
}
