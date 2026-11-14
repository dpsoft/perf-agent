package perfmcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// storeServer is a server over a root holding CPUBasic (a), CPUBasicNew (b)
// and HugeValue (c), with at most max open handles.
func storeServer(t *testing.T, max int) *Server {
	t.Helper()
	cfg := testConfig(t)
	cfg.Limits.MaxOpenHandles = max
	writeFixture(t, cfg.ProfilesDir, "a.pb.gz", pmfixture.CPUBasic())
	writeFixture(t, cfg.ProfilesDir, "b.pb.gz", pmfixture.CPUBasicNew())
	writeFixture(t, cfg.ProfilesDir, "c.pb.gz", pmfixture.HugeValue())
	srv := must(New(cfg))(t)
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

const cpuView = "cpu/nanoseconds#0"

func TestTheStoreEvictsTheLeastRecentlyUsedHandle(t *testing.T) {
	srv := storeServer(t, 1)
	a := must(srv.store.open("a.pb.gz", ""))(t)
	b := must(srv.store.open("b.pb.gz", ""))(t)
	_, _, err := srv.store.view(a.prof.Handle, cpuView)
	if err == nil || !strings.Contains(err.Error(), "call open(path) again") || !strings.Contains(err.Error(), "MaxOpenHandles=1") {
		t.Errorf("view on an evicted handle: %v", err)
	}
	if _, v, err := srv.store.view(b.prof.Handle, cpuView); err != nil || v.Total != 1000 {
		t.Errorf("view on the live handle: %v", err)
	}
	// Reopening brings it back.
	must(srv.store.open("a.pb.gz", ""))(t)
	if _, _, err := srv.store.view(a.prof.Handle, cpuView); err != nil {
		t.Errorf("view after reopening: %v", err)
	}
}

func TestTheStoreKeepsRecentlyUsedHandles(t *testing.T) {
	srv := storeServer(t, 2)
	a := must(srv.store.open("a.pb.gz", ""))(t)
	b := must(srv.store.open("b.pb.gz", ""))(t)
	if _, _, err := srv.store.view(a.prof.Handle, cpuView); err != nil {
		t.Fatal(err)
	}
	must(srv.store.open("c.pb.gz", ""))(t)
	if _, _, err := srv.store.view(a.prof.Handle, cpuView); err != nil {
		t.Errorf("recently used a was evicted: %v", err)
	}
	if _, _, err := srv.store.view(b.prof.Handle, cpuView); err == nil {
		t.Error("least recently used b was kept")
	}
	if n := len(srv.store.entries); n != 2 {
		t.Errorf("store holds %d entries, want 2", n)
	}
}

func TestTheStoreReusesAnOpenHandle(t *testing.T) {
	srv := storeServer(t, 8)
	a := must(srv.store.open("a.pb.gz", ""))(t)
	_, v1 := must2(t)(srv.store.view(a.prof.Handle, cpuView))
	again := must(srv.store.open("a.pb.gz", ""))(t)
	if again != a {
		t.Error("reopening the same evidence made a second entry")
	}
	_, v2 := must2(t)(srv.store.view(a.prof.Handle, cpuView))
	if v1 != v2 {
		t.Error("the view was rebuilt instead of cached")
	}
	if len(srv.store.order) != 1 || len(srv.store.entries) != 1 {
		t.Errorf("order %v entries %d", srv.store.order, len(srv.store.entries))
	}
}

func TestTheStoreRecordsEveryOpenedInput(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxOpenHandles = 1
	writeFixture(t, cfg.ProfilesDir, "a.pb.gz", pmfixture.CPUBasic())
	writeFixture(t, cfg.ProfilesDir, "gpu.pb.gz", pmfixture.GPUBasic())
	writeFixture(t, cfg.ProfilesDir, "gpu.execlog", pmfixture.GPUBasicExeclog(t))
	srv := must(New(cfg))(t)
	t.Cleanup(func() { _ = srv.Close() })

	must(srv.store.open("a.pb.gz", ""))(t)
	e := must(srv.store.open("gpu.pb.gz", "gpu.execlog"))(t)
	must(srv.store.open("a.pb.gz", ""))(t) // evicts gpu, reopens a
	if len(e.opened) != 2 {
		t.Errorf("entry opened %d files, want 2", len(e.opened))
	}
	if len(srv.store.inputs) != 3 {
		t.Fatalf("inputs = %d, want 3 distinct files (eviction keeps them)", len(srv.store.inputs))
	}
	for _, name := range []string{"a.pb.gz", "gpu.pb.gz", "gpu.execlog"} {
		fi := must(os.Stat(filepath.Join(cfg.ProfilesDir, name)))(t)
		found := false
		for _, in := range srv.store.inputs {
			found = found || os.SameFile(in, fi)
		}
		if !found {
			t.Errorf("%s is not among the inputs", name)
		}
	}
}

func TestTheStoreNamesTheViewsOfAHandle(t *testing.T) {
	srv := storeServer(t, 8)
	a := must(srv.store.open("a.pb.gz", ""))(t)
	for _, bad := range []string{"cpu/nanoseconds#1", "cpu/nanoseconds", "gpu/nanoseconds#0", "cpu/nanoseconds#00"} {
		_, _, err := srv.store.view(a.prof.Handle, bad)
		if err == nil || !strings.Contains(err.Error(), cpuView) {
			t.Errorf("view %q: %v", bad, err)
		}
	}
	if _, _, err := srv.store.view(profilemodel.Handle("nope"), cpuView); err == nil || !strings.Contains(err.Error(), "call open") {
		t.Errorf("unknown handle: %v", err)
	}
}

func TestViewNamesAndParse(t *testing.T) {
	p := must(profilemodel.FromBytes(pmfixture.GPUBasic(), nil, profilemodel.DefaultLimits()))(t)
	names := viewNames(p)
	if len(names) != 1 || names[0] != "gpu/nanoseconds#0" {
		t.Errorf("viewNames = %v", names)
	}
	if i, err := parseView(p, "gpu/nanoseconds#0"); err != nil || i != 0 {
		t.Errorf("parseView = %d, %v", i, err)
	}
	if _, err := parseView(p, "cpu/nanoseconds#0"); err == nil {
		t.Error("parseView accepted a view the profile does not have")
	}
}

// must2 is must for two results and an error.
func must2(t *testing.T) func(*entry, *profilemodel.View, error) (*entry, *profilemodel.View) {
	return func(e *entry, v *profilemodel.View, err error) (*entry, *profilemodel.View) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return e, v
	}
}
