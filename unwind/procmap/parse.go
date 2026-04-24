package procmap

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// parseMapsFile reads a /proc/<pid>/maps file and returns executable
// file-backed mappings sorted by Start. Non-executable ranges,
// anonymous mappings, and special pseudo-files ([heap], [stack],
// [vdso], [vvar], [vsyscall]) are skipped — PCs inside them have no
// meaningful ELF identity.
func parseMapsFile(path string) ([]Mapping, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Mapping
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		m, ok, err := parseMapsLine(line)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", line, err)
		}
		if ok {
			out = append(out, m)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}

// parseMapsLine parses one line of /proc/<pid>/maps. Returns ok=false
// for non-executable, anonymous, or pseudo-file lines (caller should
// skip them without emitting a Mapping). Returns an error only for
// malformed lines.
func parseMapsLine(line string) (Mapping, bool, error) {
	// addr_range perms offset dev inode pathname
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return Mapping{}, false, fmt.Errorf("too few fields: %d", len(fields))
	}

	perms := fields[1]
	if len(perms) < 3 || perms[2] != 'x' {
		return Mapping{}, false, nil
	}

	var path string
	if len(fields) >= 6 {
		path = fields[5]
	}
	if path == "" || strings.HasPrefix(path, "[") {
		return Mapping{}, false, nil
	}

	dash := strings.IndexByte(fields[0], '-')
	if dash < 0 {
		return Mapping{}, false, fmt.Errorf("no dash in range %q", fields[0])
	}
	start, err := strconv.ParseUint(fields[0][:dash], 16, 64)
	if err != nil {
		return Mapping{}, false, fmt.Errorf("start: %w", err)
	}
	limit, err := strconv.ParseUint(fields[0][dash+1:], 16, 64)
	if err != nil {
		return Mapping{}, false, fmt.Errorf("limit: %w", err)
	}
	off, err := strconv.ParseUint(fields[2], 16, 64)
	if err != nil {
		return Mapping{}, false, fmt.Errorf("offset: %w", err)
	}

	return Mapping{Path: path, Start: start, Limit: limit, Offset: off, IsExec: true}, true, nil
}
