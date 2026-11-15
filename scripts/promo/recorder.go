package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

const (
	fps = 15
	// The page is laid out at 960x540 CSS pixels and captured at 4/3: a
	// 1280x720 video in which everything is a third larger, readable on X.
	cssWidth, cssHeight = 960, 540
	scale               = 4.0 / 3
)

// overlay adds what a screen recording would show and a headless browser does
// not: a pointer, a ripple on click and the caption pill. It also records each
// flame graph frame's original position, so a frame can be found again after
// a zoom has moved it, and room below the page, so the deepest frames can
// scroll clear of the caption. Nothing here changes how the page behaves.
const overlay = `(() => {
  if (window.__pa) return;
  if (document.querySelector('.frame')) document.body.style.paddingBottom = '200px';
  const add = (id, css, html) => { const e = document.createElement('div'); e.id = id; e.style.cssText = css; e.innerHTML = html || ''; document.body.appendChild(e); return e; };
  const cur = add('__pa_cursor', 'position:fixed;z-index:100000;left:-40px;top:0;width:22px;height:22px;pointer-events:none;transform:translate(-2px,-2px)',
    '<svg width="22" height="22" viewBox="0 0 22 22"><path d="M3 2 L3 18 L7.5 14 L10.5 20.5 L13 19.4 L10.1 13 L16 13 Z" fill="#111" stroke="#fff" stroke-width="1.6" stroke-linejoin="round"/></svg>');
  const rip = add('__pa_ripple', 'position:fixed;z-index:99999;width:30px;height:30px;margin:-15px 0 0 -15px;border-radius:50%;border:3px solid #e8873a;background:rgb(232 135 58/.25);pointer-events:none;display:none');
  document.querySelectorAll('.frame').forEach(f => { f.dataset.ol = parseFloat(f.style.left); });
  const box = (el, dx) => { const b = el.getBoundingClientRect(); return {x: b.left + Math.min(b.width / 2, dx), y: b.top + b.height / 2 + scrollY, w: b.width, left: b.left, right: b.right, name: el.textContent}; };
  const visible = name => [...document.querySelectorAll('.frame')].filter(f => f.textContent.startsWith(name) && f.getBoundingClientRect().width > 0);
  const widest = els => els.sort((a, b) => b.getBoundingClientRect().width - a.getBoundingClientRect().width)[0];
  window.__pa = {
    cursor(x, y) { cur.style.left = x + 'px'; cur.style.top = y + 'px'; },
    hideCursor() { cur.style.left = '-40px'; },
    ripple(x, y, on) { rip.style.left = x + 'px'; rip.style.top = y + 'px'; rip.style.display = on ? 'block' : 'none'; },
    caption(t, s) {
      let c = document.getElementById('__pa_caption');
      if (!c) c = add('__pa_caption', '');
      c.style.cssText = 'position:fixed;left:28px;bottom:36px;z-index:99998;background:#0d1117ee;color:#fff;padding:10px 18px;border-radius:10px;font:600 24px ui-sans-serif,system-ui,sans-serif;box-shadow:0 6px 24px #0006;max-width:820px';
      c.innerHTML = t + '<div style="font-weight:400;font-size:15px;color:#b6bec8;margin-top:4px">' + s + '</div>';
    },
    // The widest visible frame whose name starts with name and whose original
    // left edge, in percent of the graph, lies in [lo, hi].
    frame(name, lo, hi, dx) {
      const el = widest(visible(name).filter(f => +f.dataset.ol >= lo && +f.dataset.ol <= hi));
      return el ? box(el, dx) : null;
    },
    // The widest visible frame whose name starts with name and that spans x.
    over(name, x, dx) {
      const el = widest(visible(name).filter(f => { const b = f.getBoundingClientRect(); return b.left <= x && b.right >= x; }));
      return el ? box(el, dx) : null;
    },
    hovered() { const f = document.querySelector('.frame:hover'); return f ? f.textContent : ''; },
    graphTop() { const f = document.querySelector('.frame'); return f ? f.getBoundingClientRect().top + scrollY : 0; },
    maxScroll() { return document.documentElement.scrollHeight - innerHeight; },
  };
})()`

// pt is a point with x in viewport pixels and y in document pixels.
type pt struct {
	X, Y, W, Left, Right float64
	Name                 string
}

type mark struct {
	name  string
	frame int
}

type recorder struct {
	ctx    context.Context
	dir    string
	n      int
	mx, my float64 // pointer, viewport pixels
	marks  []mark
}

// errStop carries a failed browser action out of a scene.
type errStop struct{ err error }

func newRecorder(chrome, dir string) (*recorder, func(), error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.WindowSize(cssWidth, cssHeight),
		// The scale is the browser's own, not an emulated one: with
		// Emulation.setDeviceMetricsOverride scaling, a screenshot re-runs hit
		// testing at the pointer's position in device pixels, so hover state
		// and tooltips in the capture belong to another frame.
		chromedp.Flag("force-device-scale-factor", fmt.Sprint(scale)),
	)
	actx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelCtx := chromedp.NewContext(actx)
	ctx, cancelTimeout := context.WithTimeout(ctx, 10*time.Minute)
	stop := func() { cancelTimeout(); cancelCtx(); cancelAlloc() }
	// The window size includes the headless browser's frame; pin the viewport.
	err := chromedp.Run(ctx, emulation.SetDeviceMetricsOverride(cssWidth, cssHeight, 0, false))
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("start %s: %w", chrome, err)
	}
	return &recorder{ctx: ctx, dir: dir, mx: cssWidth * 0.6, my: cssHeight * 0.45}, stop, nil
}

// try runs the scenes, turning a failed browser action into an error.
func (r *recorder) try(scenes func()) (err error) {
	defer func() {
		if p := recover(); p != nil {
			stop, ok := p.(errStop)
			if !ok {
				panic(p)
			}
			err = stop.err
		}
	}()
	scenes()
	return nil
}

func (r *recorder) do(actions ...chromedp.Action) {
	if err := chromedp.Run(r.ctx, actions...); err != nil {
		panic(errStop{err})
	}
}

func (r *recorder) eval(js string, out any) { r.do(chromedp.Evaluate(js, out)) }

func (r *recorder) call(fn string, args ...any) {
	b, _ := json.Marshal(append([]any{}, args...))
	r.eval(fmt.Sprintf("__pa.%s(...%s)", fn, b), nil)
}

func (r *recorder) scene(name string) { r.marks = append(r.marks, mark{name, r.n}) }

// snap captures the viewport and holds it for the given seconds (one frame at least).
func (r *recorder) snap(seconds float64) {
	var png []byte
	r.do(chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		png, err = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).Do(ctx)
		return err
	}))
	first := filepath.Join(r.dir, fmt.Sprintf("f%05d.png", r.n))
	if err := os.WriteFile(first, png, 0o644); err != nil {
		panic(errStop{err})
	}
	r.n++
	for range max(1, int(seconds*fps+0.5)) - 1 {
		next := filepath.Join(r.dir, fmt.Sprintf("f%05d.png", r.n))
		if err := os.Link(first, next); err != nil {
			if err := os.WriteFile(next, png, 0o644); err != nil {
				panic(errStop{err})
			}
		}
		r.n++
	}
}

func (r *recorder) frame() { r.snap(1.0 / fps) }

// open navigates and waits for the selector, then adds the overlay.
func (r *recorder) open(url, ready string) {
	r.do(chromedp.Navigate(url), chromedp.WaitVisible(ready, chromedp.ByQuery), chromedp.Sleep(400*time.Millisecond))
	r.eval(overlay, nil)
}

func (r *recorder) caption(title, sub string) { r.call("caption", title, sub) }

func (r *recorder) scrollY() float64 {
	var y float64
	r.eval("scrollY", &y)
	return y
}

func (r *recorder) scrollTo(y float64) { r.eval(fmt.Sprintf("scrollTo(0,%f)", y), nil) }

func (r *recorder) mouse(t input.MouseType) {
	ev := input.DispatchMouseEvent(t, r.mx, r.my)
	if t != input.MouseMoved {
		ev = ev.WithButton(input.Left).WithClickCount(1)
	}
	r.do(ev)
}

// glide moves the pointer to p in steps, one frame each, dispatching real
// mouse events on the way. When p is outside the comfortable middle of the
// viewport the page scrolls along with the pointer.
func (r *recorder) glide(p pt, steps int) {
	sy := r.scrollY()
	ty := sy
	// Keep the target in the upper middle, so its tooltip clears the caption.
	if v := p.Y - sy; v < 12 || v > 300 {
		var maxScroll float64
		r.eval("__pa.maxScroll()", &maxScroll)
		ty = min(max(0, p.Y-200), maxScroll)
	}
	sx, smy := r.mx, r.my
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		e := t * t * (3 - 2*t)
		if ty != sy {
			r.scrollTo(sy + (ty-sy)*e)
		}
		r.mx, r.my = sx+(p.X-sx)*e, smy+(p.Y-ty-smy)*e
		r.mouse(input.MouseMoved)
		r.call("cursor", r.mx, r.my)
		r.frame()
	}
}

func (r *recorder) click() {
	r.call("ripple", r.mx, r.my, true)
	r.mouse(input.MousePressed)
	r.snap(2.0 / fps)
	r.mouse(input.MouseReleased)
	r.call("ripple", 0, 0, false)
}

func (r *recorder) key(k string) { r.do(chromedp.KeyEvent(k)) }

func (r *recorder) escape() { r.key(kb.Escape) }

// ctrlF opens the flame graph's search box.
func (r *recorder) ctrlF() {
	r.do(input.DispatchKeyEvent(input.KeyRawDown).WithKey("f").WithCode("KeyF").
		WithWindowsVirtualKeyCode(70).WithModifiers(input.ModifierCtrl))
	r.do(input.DispatchKeyEvent(input.KeyUp).WithKey("f").WithCode("KeyF").
		WithWindowsVirtualKeyCode(70).WithModifiers(input.ModifierCtrl))
}

// find locates a flame graph frame or stops the recording: a storyboard that
// names a frame the capture does not have would otherwise film the wrong thing.
func (r *recorder) find(js string) pt {
	var p *pt
	r.eval(js, &p)
	if p == nil {
		panic(errStop{errors.New("not in the capture: " + js)})
	}
	return *p
}

// hover glides onto a frame found by js and checks the page agrees the
// pointer is on it, so a caption never names a frame the pointer missed.
func (r *recorder) hover(js string, steps int) pt {
	p := r.find(js)
	r.glide(p, steps)
	var got string
	r.eval("__pa.hovered()", &got)
	if got != p.Name {
		panic(errStop{fmt.Errorf("pointer at %.0f,%.0f is on %q, not on %q", r.mx, r.my, got, p.Name)})
	}
	return p
}
