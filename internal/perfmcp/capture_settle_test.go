package perfmcp

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// undecidedEnv is an env whose manifest links answer EIO, and whose
// manifest names cannot be read, while broken is set; made says whether
// such a link is made all the same.
type undecidedEnv struct {
	*jobEnv
	mu     sync.Mutex
	broken bool
}

func newUndecidedEnv(t *testing.T, made bool) *undecidedEnv {
	u := &undecidedEnv{}
	u.jobEnv = newJobEnv(t, jobOpts{setup: func(s *Server) {
		s.link = func(r *os.Root, oldname, newname string) error {
			if !u.isBroken(newname) {
				return r.Link(oldname, newname)
			}
			if made {
				if err := r.Link(oldname, newname); err != nil {
					return err
				}
			}
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EIO}
		}
	}})
	landedLstat = func(r *os.Root, name string) (os.FileInfo, error) {
		if u.isBroken(name) {
			return nil, syscall.EIO
		}
		return r.Lstat(name)
	}
	t.Cleanup(func() { landedLstat = (*os.Root).Lstat })
	return u
}

func (u *undecidedEnv) isBroken(name string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.broken && strings.HasSuffix(name, manifestSuffix)
}

func (u *undecidedEnv) setBroken(b bool) {
	u.mu.Lock()
	u.broken = b
	u.mu.Unlock()
}

// undecided starts a compare with 001 whose commit ends undecided.
func (u *undecidedEnv) undecided(t *testing.T) *job {
	t.Helper()
	if _, v := u.run(u.request(300 * time.Millisecond)); v.Status != string(jobDone) {
		t.Fatalf("base: %+v", v)
	}
	u.setBroken(true)
	j := u.start(u.compareRequest("001"))
	if !eventuallyTrue(30*time.Second, func() bool { return j.view().Phase == phaseUndecided }) {
		t.Fatalf("view %+v, err %v", j.view(), jobErr(j))
	}
	if !eventuallyTrue(5*time.Second, func() bool { return u.s.registry.active.Load() == nil }) {
		t.Fatal("an undecided job holds the slot")
	}
	return j
}

// An undecided job answers a cancel as pending, from itself, the registry
// and its alias, and holds no error yet. When it is settled, the cancel is
// reported as §7.4 says: too late for a commit that was made, cancelled
// for one that was not. A committed one keeps its baseline in the recipe.
func TestAnUndecidedJobAnswersPendingAndReportsTheCancel(t *testing.T) {
	for _, made := range []bool{true, false} {
		t.Run(map[bool]string{true: "made", false: "not made"}[made], func(t *testing.T) {
			u := newUndecidedEnv(t, made)
			j := u.undecided(t)
			if ce := jobErr(j); ce != nil {
				t.Errorf("an undecided job holds the error %v", ce)
			}
			// The cancel comes through the registry for one, the job for
			// the other, so each route is shown to record it.
			if made {
				if a, _ := u.s.registry.cancel(j.id); a != cancelAnswerPending {
					t.Errorf("registry cancel %q", a)
				}
			} else if a := j.requestCancel(); a != cancelAnswerPending {
				t.Errorf("job cancel %q", a)
			}
			u.s.registry.mu.Lock()
			alias := u.s.registry.aliases[j.id].answer
			u.s.registry.mu.Unlock()
			if alias != cancelAnswerPending {
				t.Errorf("alias answer %q", alias)
			}
			u.setBroken(false)
			if next, v := u.run(u.request(300 * time.Millisecond)); v.Status != string(jobDone) {
				t.Fatalf("next: %+v, err %v", v, jobErr(next))
			}
			v := j.view()
			if made {
				if v.Status != string(jobDone) || v.Derived != derivedRecompute {
					t.Fatalf("settled %+v, err %v", v, jobErr(j))
				}
				r := u.result(j)
				if r.Cancel != cancelAnswerTooLate || r.Recompute == nil || r.Recompute.Baseline != "present" {
					t.Errorf("cancel %q, recompute %+v", r.Cancel, r.Recompute)
				}
				if a, _ := u.s.registry.cancel(j.id); a != cancelAnswerTooLate {
					t.Errorf("registry cancel after the settle %q", a)
				}
				return
			}
			if ce := jobErr(j); v.Status != string(jobCancelled) || ce == nil || ce.Code != capture.CodeCancelled {
				t.Fatalf("settled %+v, err %v", v, ce)
			}
			if m, ok := u.manifestOf("adhoc", "002"); !ok || m.Error == nil || m.Error.Code != string(capture.CodeCancelled) {
				t.Errorf("manifest %+v", m)
			}
			if a, _ := u.s.registry.cancel(j.id); a != cancelAnswerCancelled {
				t.Errorf("registry cancel after the settle %q", a)
			}
		})
	}
}

// A manifest that cannot be read when the commit is settled leaves the job
// undecided for the next pass, which settles it.
func TestATransientReadKeepsTheJobUndecided(t *testing.T) {
	u := newUndecidedEnv(t, true)
	j := u.undecided(t)
	var fails atomic.Int32
	fails.Store(1)
	settleRead = func(ser *os.Root, name string) (manifest, error) {
		if fails.Add(-1) >= 0 {
			return manifest{}, &os.PathError{Op: "open", Path: name, Err: syscall.EIO}
		}
		return readManifest(ser, name)
	}
	t.Cleanup(func() { settleRead = readManifest })
	u.setBroken(false)
	u.run(u.request(300 * time.Millisecond))
	if v := j.view(); v.Status != string(jobCommitting) || v.Phase != phaseUndecided {
		t.Fatalf("after a failed read %+v", v)
	}
	u.run(u.request(300 * time.Millisecond))
	if v := j.view(); v.Status != string(jobDone) || v.Derived != derivedRecompute {
		t.Fatalf("after a good read %+v, err %v", v, jobErr(j))
	}
}
