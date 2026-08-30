package pyunwind

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ErrUnsupportedVersion is returned for an interpreter we detected but will
// not walk. It is deliberately distinct from "not an interpreter": an
// operator seeing Python frames missing deserves to know we found 3.11 and
// declined, not that we found nothing.
var ErrUnsupportedVersion = errors.New("pyunwind: unsupported CPython version")

type Version struct{ Major, Minor, Micro int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Micro) }

// Supported reports whether this build has an offset table and a TSS parser
// that covers it. 3.12 is the floor: 3.11 has no _PyThreadState_GetCurrent
// and a different PyGILState shape (see the spec's non-goals).
func (v Version) Supported() bool { return v.Major == 3 && v.Minor >= 12 && v.Minor <= 14 }

var sonameRe = regexp.MustCompile(`(?:libpython|python)(\d+)\.(\d+)`)

// DetectFromSoname reads the version out of a mapped path. Cheap and works
// on stripped binaries, which is why it is tried first.
func DetectFromSoname(path string) (Version, bool) {
	m := sonameRe.FindStringSubmatch(path)
	if m == nil {
		return Version{}, false
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return Version{}, false
	}
	return Version{Major: major, Minor: minor}, true
}

// DetectFromPyVersionHex decodes PY_VERSION_HEX, used when the path carries
// no version (a bare `python`, or an embedded interpreter).
func DetectFromPyVersionHex(hex uint32) Version {
	return Version{
		Major: int((hex >> 24) & 0xff),
		Minor: int((hex >> 16) & 0xff),
		Micro: int((hex >> 8) & 0xff),
	}
}
