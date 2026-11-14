package explain

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// maxClaimIDLen is the longest claim ID: a letter and up to 31 more.
const maxClaimIDLen = 32

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
// is text. A number starts at a digit (in any script) not preceded by a
// letter, digit, "_" or ".", continues over digits and over a "." or ","
// followed by a digit, and takes one trailing "%"; it becomes an
// unchecked_number segment, so a figure no claim checked is never shown as
// if it were one. Everything else is text, with adjacent text merged.
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
		if unicode.IsDigit(r) && numberMayStart(s[:i]) {
			n := numberLen(s[i:])
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
	if id, ok := strings.CutPrefix(inner, "n:"); ok {
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

// numberMayStart reports whether a number may start right after before: not
// after a letter, digit, "_" or ".", so "sm86", "v2", "x_1" and ".5" are
// not numbers.
func numberMayStart(before string) bool {
	if before == "" {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(before)
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '.'
}

// numberLen is the length in bytes of the number at the start of s, which
// starts with a digit.
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

// digitsLen is the length in bytes of the run of digits at the start of s.
func digitsLen(s string) int {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsDigit(r) {
			break
		}
		i += size
	}
	return i
}
