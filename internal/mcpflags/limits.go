package mcpflags

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// LimitFields maps every settable limit name ("MaxRows", "Model.MaxRawBytes")
// to its field in l.
func LimitFields(l *perfmcp.Limits) map[string]reflect.Value {
	out := map[string]reflect.Value{}
	var walk func(prefix string, v reflect.Value)
	walk = func(prefix string, v reflect.Value) {
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			name := prefix + f.Name
			switch {
			case f.Type == durationType:
			case f.Type.Kind() == reflect.Struct:
				walk(name+".", v.Field(i))
				continue
			case isSignedInt(f.Type.Kind()):
			default:
				panic("mcpflags: limit " + name + " has a type --limit cannot set: " + f.Type.String())
			}
			out[name] = v.Field(i)
		}
	}
	walk("", reflect.ValueOf(l).Elem())
	return out
}

var durationType = reflect.TypeFor[time.Duration]()

// An init-time check that every field of perfmcp.Limits has a type --limit
// can set: LimitFields panics otherwise, so a new field of another type
// cannot go unnoticed.
var _ = LimitFields(&perfmcp.Limits{})

func isSignedInt(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

// zeroLimits are the limits that may be zero: Source.MaxContext (no
// context lines) and Capture.TTL (no age limit).
var zeroLimits = []string{"Source.MaxContext", "Capture.TTL"}

// CheckLimits refuses a limit that is not positive, naming it; the server
// would otherwise replace a zero with a default without saying so.
// zeroLimits may be zero.
func CheckLimits(l *perfmcp.Limits) error {
	for name, f := range LimitFields(l) {
		zero := slices.Contains(zeroLimits, name)
		if f.Int() > 0 || (f.Int() == 0 && zero) {
			continue
		}
		if zero {
			return fmt.Errorf("limit %s must not be negative (0 is allowed), got %d", name, f.Int())
		}
		return fmt.Errorf("limit %s must be positive, got %d", name, f.Int())
	}
	return nil
}

// SetLimit applies one Name=Value to l.
func SetLimit(l *perfmcp.Limits, kv string) error { return setLimit(l, kv, Hub) }

// setLimit is SetLimit for the binary kind: an unknown name is answered
// with the names kind can set, so stdio's list leaves out the Hub ones.
func setLimit(l *perfmcp.Limits, kv string, kind Kind) error {
	name, val, ok := strings.Cut(kv, "=")
	if !ok {
		return errors.New("want Name=Value")
	}
	fields := LimitFields(l)
	f, ok := fields[name]
	if !ok {
		names := make([]string, 0, len(fields))
		for n := range fields {
			if kind == Hub || !strings.HasPrefix(n, hubLimitPrefix) {
				names = append(names, n)
			}
		}
		slices.Sort(names)
		return fmt.Errorf("unknown limit %q; valid names: %s", name, strings.Join(names, ", "))
	}
	if f.Type() == durationType {
		d, err := time.ParseDuration(val)
		if err != nil {
			return err
		}
		f.SetInt(int64(d))
		return nil
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if errors.Is(err, strconv.ErrRange) {
		return fmt.Errorf("%q is out of range for %s", val, name)
	}
	if err != nil {
		return fmt.Errorf("%q is not an integer", val)
	}
	if f.OverflowInt(n) {
		return fmt.Errorf("%d is out of range for %s", n, name)
	}
	f.SetInt(n)
	return nil
}

// CheckCapturesDir checks --captures against the profiles root and returns
// it relative to the root. An absolute dir must lie inside the root, as
// given or with its symlinks resolved; a relative one is relative to the
// root and must stay inside it. What of it exists is followed through the
// root, as the server opens it, so no symlink leads outside, and must be a
// directory. A root that cannot be opened is left for the server to refuse.
func CheckCapturesDir(profiles, dir string) (string, error) {
	rel := dir
	if filepath.IsAbs(dir) {
		rel = ""
		var bases []string
		if abs, err := filepath.Abs(profiles); err == nil {
			bases = append(bases, abs)
			if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
				bases = append(bases, real)
			}
		}
		for _, b := range bases {
			if r, err := filepath.Rel(b, dir); err == nil && filepath.IsLocal(r) {
				rel = r
				break
			}
		}
	}
	if rel == "" || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("it is outside the profiles root %s; name a directory inside it", profiles)
	}
	if rel = filepath.Clean(rel); rel == "." {
		return "", fmt.Errorf("it is the profiles root itself %s; name a subdirectory of it, such as captures", profiles)
	}
	root, err := os.OpenRoot(profiles)
	if err != nil {
		return rel, nil
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Stat(rel)
	self, serr := root.Stat(".")
	switch {
	case err == nil && !fi.IsDir():
		return "", errors.New("it is not a directory; name a directory inside the profiles root")
	case err == nil && serr == nil && os.SameFile(fi, self):
		return "", fmt.Errorf("it is the profiles root itself %s, through a link; name a subdirectory of it, such as captures", profiles)
	case err == nil, errors.Is(err, fs.ErrNotExist):
		return rel, nil
	case errors.Is(err, fs.ErrPermission):
		return "", fmt.Errorf("it cannot be checked: %v", err)
	}
	return "", fmt.Errorf("it leads outside the profiles root %s through a symlink (%v); name a directory inside it", profiles, err)
}
