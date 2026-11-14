package perfmcp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// entry is one open handle: the loaded profile, the views built from it so
// far and the files it was read from.
type entry struct {
	prof   *profilemodel.Profile
	name   string // the profile's path relative to the profiles root, as first opened
	views  map[string]*profilemodel.View
	opened []os.FileInfo
}

// store holds a server's open handles. It belongs to one Server and is used
// from its single request goroutine, so it takes no locks; there is no cache
// shared between servers, so each server judges evidence by its own limits.
type store struct {
	root *os.Root
	lim  profilemodel.Limits
	max  int
	// entries are the open handles; order lists them least recently used
	// first.
	entries map[profilemodel.Handle]*entry
	order   []profilemodel.Handle
	// inputs is every distinct file the server has read a profile or an
	// executions log from, kept after its handle is evicted: a page the
	// server writes must never replace one of them.
	inputs []os.FileInfo
}

func newStore(root *os.Root, lim profilemodel.Limits, max int) *store {
	return &store{root: root, lim: lim, max: max, entries: map[profilemodel.Handle]*entry{}}
}

// fileFS serves already opened files under fixed names, so LoadFS reads the
// very handles whose type openRegular checked. Close is the store's job.
type fileFS map[string]*os.File

type keptOpen struct{ *os.File }

func (keptOpen) Close() error { return nil }

func (m fileFS) Open(name string) (fs.File, error) {
	f, ok := m[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return keptOpen{f}, nil
}

// open loads name (and execlog, if not "") from the profiles root, both
// relative to it, and returns its entry. Opening evidence that is already
// open returns the existing entry. A missing execlog, a load error or a
// default view that cannot be built leaves the store as it was.
func (s *store) open(name, execlog string) (_ *entry, err error) {
	pf, pfi, err := openRegular(s.root, rootProfiles, "path", name)
	if err != nil {
		return nil, err
	}
	files := fileFS{"profile": pf}
	opened := []os.FileInfo{pfi}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	logName := ""
	if execlog != "" {
		lf, lfi, err := openRegular(s.root, rootProfiles, "execlog", execlog)
		if err != nil {
			return nil, err
		}
		files["execlog"], logName = lf, "execlog"
		opened = append(opened, lfi)
	}

	p, err := profilemodel.LoadFS(files, "profile", logName, s.lim)
	if err != nil {
		if errors.Is(err, profilemodel.ErrLimit) {
			return nil, fmt.Errorf("cannot open %q: %v; this server's limits are fixed when it starts, so it cannot open this profile", name, err)
		}
		return nil, fmt.Errorf("cannot open %q: %v; pass a pprof profile written by perf-agent", name, err)
	}
	s.record(opened)
	if e, ok := s.entries[p.Handle]; ok {
		s.touch(p.Handle)
		return e, nil
	}

	if len(p.Source.SampleType) == 0 {
		return nil, fmt.Errorf("cannot open %q: the profile declares no sample types, so it has no view", name)
	}
	e := &entry{prof: p, name: name, views: map[string]*profilemodel.View{}, opened: opened}
	def := defaultView(p)
	v, err := p.View(profilemodel.ViewOptions{SampleIndex: foldedstacks.ChooseSampleIndex(p.Source)})
	if err != nil {
		return nil, fmt.Errorf("cannot build view %s of %q: %v", def, name, err)
	}
	e.views[def] = v
	s.entries[p.Handle] = e
	s.order = append(s.order, p.Handle)
	for len(s.order) > s.max {
		delete(s.entries, s.order[0])
		s.order = slices.Delete(s.order, 0, 1)
	}
	return e, nil
}

// record adds the files not already among the inputs.
func (s *store) record(files []os.FileInfo) {
	for _, fi := range files {
		if !slices.ContainsFunc(s.inputs, func(in os.FileInfo) bool { return os.SameFile(in, fi) }) {
			s.inputs = append(s.inputs, fi)
		}
	}
}

// touch marks h as the most recently used handle.
func (s *store) touch(h profilemodel.Handle) {
	if i := slices.Index(s.order, h); i >= 0 {
		s.order = append(slices.Delete(s.order, i, i+1), h)
	}
}

// view returns handle h's entry and its view named view, building and
// caching the view on first use.
func (s *store) view(h profilemodel.Handle, view string) (*entry, *profilemodel.View, error) {
	e, ok := s.entries[h]
	if !ok {
		return nil, nil, fmt.Errorf("handle %s is not open in this server (it keeps the MaxOpenHandles=%d most recently used); call open(path) again to get a handle", h, s.max)
	}
	s.touch(h)
	if v, ok := e.views[view]; ok {
		return e, v, nil
	}
	idx, err := parseView(e.prof, view)
	if err != nil {
		return nil, nil, fmt.Errorf("handle %s: %w", h, err)
	}
	v, err := e.prof.View(profilemodel.ViewOptions{SampleIndex: idx})
	if err != nil {
		return nil, nil, fmt.Errorf("cannot build view %s of handle %s: %v", view, h, err)
	}
	e.views[view] = v
	return e, v, nil
}

// viewNames are p's views, "<sample type>/<unit>#<index>", in sample type
// order.
func viewNames(p *profilemodel.Profile) []string {
	names := make([]string, len(p.Source.SampleType))
	for i, st := range p.Source.SampleType {
		names[i] = fmt.Sprintf("%s/%s#%d", st.Type, st.Unit, i)
	}
	return names
}

// parseView returns the sample index of the view named name, which must be
// one of viewNames(p) exactly.
func parseView(p *profilemodel.Profile, name string) (int, error) {
	names := viewNames(p)
	if i := slices.Index(names, name); i >= 0 {
		return i, nil
	}
	return 0, fmt.Errorf("view %q is not a view of this profile; pass one of the views open returned: %s", name, strings.Join(names, ", "))
}

// defaultView is the view the flame graph shows by default. p declares at
// least one sample type.
func defaultView(p *profilemodel.Profile) string {
	return viewNames(p)[foldedstacks.ChooseSampleIndex(p.Source)]
}
