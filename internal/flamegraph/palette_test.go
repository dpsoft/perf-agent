package flamegraph

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The palette now lives in CSS rather than in Go string literals, which buys
// a theme-aware graph and costs a compile-time guarantee: a domain key and a
// selector that no longer agree would produce a page of black rectangles and
// nothing would fail to build. These tests are that guarantee, moved.

var (
	// hsl(H calc(S% * var(--fill-ds)) calc(L% + var(--fill-dl)))
	fillTokenRe = regexp.MustCompile(
		`--(fill-[a-z-]+): hsl\( ?(\d+) (?:calc\((\d+)% \* var\(--fill-ds\)\)|(\d+)%) calc\((\d+)% \+ var\(--fill-dl\)\)\);`)
	darkBlockRe = regexp.MustCompile(`(?s)@media \(prefers-color-scheme: dark\)\{\s*:root\{([^}]*)\}`)
)

type hsl struct{ h, s, l float64 }

// palette parses paletteCSS back into numbers. Reading the stylesheet is the
// point: a test that re-declared the colours in Go would agree with itself
// and not with the page.
func palette(t *testing.T) map[string]hsl {
	t.Helper()
	flat := regexp.MustCompile(`\s+`).ReplaceAllString(paletteCSS, " ")
	out := map[string]hsl{}
	for _, m := range fillTokenRe.FindAllStringSubmatch(flat, -1) {
		sat := m[3]
		if sat == "" {
			sat = m[4]
		}
		out[m[1]] = hsl{h: num(t, m[2]), s: num(t, sat), l: num(t, m[5])}
	}
	require.NotEmpty(t, out, "no fill tokens parsed out of paletteCSS")
	return out
}

func num(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(s, 64)
	require.NoError(t, err)
	return v
}

// themed applies the two knobs the dark theme is allowed to move.
func (c hsl) themed(dl, ds float64) hsl { return hsl{c.h, c.s * ds, c.l + dl} }

func (c hsl) rgb() (float64, float64, float64) {
	h, s, l := math.Mod(c.h, 360)/360, c.s/100, c.l/100
	if s == 0 {
		return l, l, l
	}
	q := l + s - l*s
	if l < 0.5 {
		q = l * (1 + s)
	}
	p := 2*l - q
	hue := func(t float64) float64 {
		t = math.Mod(t+1, 1)
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 1.0/2:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		default:
			return p
		}
	}
	return hue(h + 1.0/3), hue(h), hue(h - 1.0/3)
}

func (c hsl) luminance() float64 {
	lin := func(u float64) float64 {
		if u <= 0.04045 {
			return u / 12.92
		}
		return math.Pow((u+0.055)/1.055, 2.4)
	}
	r, g, b := c.rgb()
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrast(a, b hsl) float64 {
	la, lb := a.luminance(), b.luminance()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// overHatch composites the hatch ink over a fill at full coverage. The real
// stripes cover about a third of a frame, so this is the pessimistic reading
// of "what is behind the darkest pixel of a label".
func overHatch(c hsl, alpha float64) hsl {
	r, g, b := c.rgb()
	mix := func(v, ink float64) float64 { return v*(1-alpha) + ink*alpha }
	m := hsl{}
	// Back to a luminance-only stand-in: contrast() only reads luminance,
	// and mixing in sRGB then re-deriving lightness is what a browser draws.
	rr, gg, bb := mix(r, 22.0/255), mix(g, 20.0/255), mix(b, 18.0/255)
	maxc, minc := math.Max(rr, math.Max(gg, bb)), math.Min(rr, math.Min(gg, bb))
	m.l = (maxc + minc) / 2 * 100
	if maxc != minc {
		d := maxc - minc
		if m.l > 50 {
			m.s = d / (2 - maxc - minc) * 100
		} else {
			m.s = d / (maxc + minc) * 100
		}
		switch maxc {
		case rr:
			m.h = math.Mod((gg-bb)/d, 6) * 60
		case gg:
			m.h = ((bb-rr)/d + 2) * 60
		default:
			m.h = ((rr-gg)/d + 4) * 60
		}
	}
	return m
}

const (
	frameInkH, frameInkS, frameInkL = 30, 12, 9
	lightDL, lightDS, lightHatch    = 0.0, 1.0, 0.26
	darkDL, darkDS, darkHatch       = -9.0, 0.9, 0.20
)

func TestPaletteDeclaresATokenForEveryDomain(t *testing.T) {
	tokens := palette(t)
	for d := Domain(0); d < numDomains; d++ {
		info := d.Info()
		name := strings.TrimSuffix(strings.TrimPrefix(info.Fill, "var(--"), ")")
		assert.Contains(t, tokens, name, "domain %q names a fill token the stylesheet does not declare", info.Key)
		if info.Stroke != "" {
			assert.Contains(t, paletteCSS, strings.TrimSuffix(strings.TrimPrefix(info.Stroke, "var("), ")")+":",
				"domain %q names a stroke token the stylesheet does not declare", info.Key)
		}
	}
}

// Colour reaches a frame through data-domain and nothing else, so a key that
// no rule selects is a frame with no fill.
func TestEveryDomainKeyIsSelectedByARule(t *testing.T) {
	for d := Domain(0); d < numDomains; d++ {
		info := d.Info()
		sel := fmt.Sprintf(`g.frame[data-domain="%s"] rect.bg{fill:%s`, info.Key, info.Fill)
		assert.Contains(t, paletteCSS, sel, "no rule paints domain %q", info.Key)
	}
}

// Aqua is Gregg's accelerator-source layer. perf-agent cannot produce those
// frames yet; the hue is held open rather than spent on something else, and
// the page says so instead of leaving a silent gap in the palette.
func TestAquaIsReservedAndUnused(t *testing.T) {
	assert.Contains(t, palette(t), "fill-accel-source", "the reserved aqua token must exist")
	assert.NotContains(t, paletteCSS, "rect.bg{fill:var(--fill-accel-source)",
		"aqua must not paint any frame until accelerator source frames exist")
	for d := Domain(0); d < numDomains; d++ {
		assert.NotEqual(t, "var(--fill-accel-source)", d.Info().Fill, "domain %q took the reserved hue", d.Info().Key)
	}
}

// The whole point of the token layout: the dark theme is a lightness and a
// saturation adjustment, not a second palette. If a colour ever appears in
// the media block, the two themes have started to drift into two designs.
func TestDarkThemeMovesOnlyTheThemeKnobs(t *testing.T) {
	m := darkBlockRe.FindStringSubmatch(paletteCSS)
	require.Len(t, m, 2, "paletteCSS must have exactly one dark-theme block")
	for _, decl := range strings.Split(m[1], ";") {
		decl = strings.TrimSpace(decl)
		if decl == "" {
			continue
		}
		prop := strings.TrimSpace(strings.SplitN(decl, ":", 2)[0])
		assert.Contains(t, []string{"--fill-dl", "--fill-ds", "--hatch-ink"}, prop,
			"the dark theme may only move the knobs, but it sets %s", prop)
	}
	assert.Equal(t, 1, strings.Count(paletteCSS, "@media"), "one media block, or the themes are diverging")
}

// Gregg's layers are hues; the hue is the meaning. Anchor them, so a tweak
// to a lightness cannot quietly turn the C++ layer green.
func TestHuesAreTheGreggLayers(t *testing.T) {
	tokens := palette(t)
	for _, c := range []struct {
		token   string
		gregg   string
		hue     float64
		minSat  float64
		tolHue  float64
		neutral bool
	}{
		{token: "fill-app", gregg: "pink", hue: 335, minSat: 60, tolHue: 20},
		{token: "fill-system", gregg: "red", hue: 2, minSat: 55, tolHue: 15},
		{token: "fill-vendor", gregg: "yellow", hue: 50, minSat: 60, tolHue: 12},
		{token: "fill-kernel", gregg: "orange", hue: 28, minSat: 60, tolHue: 12},
		{token: "fill-gpu-kernel", gregg: "green", hue: 128, minSat: 30, tolHue: 25},
		{token: "fill-accel-source", gregg: "aqua", hue: 184, minSat: 30, tolHue: 20},
	} {
		got, ok := tokens[c.token]
		require.True(t, ok, c.token)
		assert.InDelta(t, c.hue, got.h, c.tolHue, "%s is Gregg's %s layer", c.token, c.gregg)
		assert.GreaterOrEqual(t, got.s, c.minSat, "%s must read as a hue, not as grey", c.token)
	}
	// The boundary markers are Gregg's grey: no hue at all, so they cannot
	// be mistaken for a layer of the computation.
	for _, n := range []string{"fill-boundary", "fill-boundary-unattributed"} {
		assert.Zero(t, tokens[n].s, "%s must be a true grey", n)
	}
	// The two domains Gregg has no layer for are drained rather than given a
	// sixth hue, and they lean opposite ways so they stay tellable apart.
	assert.Less(t, tokens["fill-unsym"].s, 30.0, "unsymbolized must be near-grey, not a hue")
	assert.Less(t, tokens["fill-shim"].s, 30.0, "the shim must be near-grey, not a hue")
	assert.Less(t, tokens["fill-unsym"].h, 90.0, "unsymbolized leans warm, with the CPU band it belongs to")
	assert.Greater(t, tokens["fill-shim"].h, 180.0, "the shim leans cool, away from the CPU band")
}

// [gpu:launch] and [gpu:launch unsampled] mean different things — GPU time we
// have a CPU stack for, versus GPU time we do not. They share Gregg's grey,
// so three other signals have to keep them apart.
func TestTheTwoBoundariesStayTellableApart(t *testing.T) {
	tokens := palette(t)
	assert.Greater(t, tokens["fill-boundary-unattributed"].l, tokens["fill-boundary"].l+5,
		"the unattributed boundary must be visibly paler")
	assert.NotEmpty(t, DomainBoundaryUnattributed.Info().Overlay, "and hatched")
	assert.Contains(t, paletteCSS, `g.frame[data-domain="boundary-unattributed"] rect.bg{fill:var(--fill-boundary-unattributed);stroke:var(--edge-unattributed);stroke-width:1;stroke-dasharray:3 2}`,
		"and dashed, because there is nothing behind it")
	assert.NotContains(t, paletteCSS, `data-domain="boundary"] rect.bg{fill:var(--fill-boundary);stroke:var(--edge-boundary);stroke-width:1;stroke-dasharray`)
}

// Frame labels are drawn on the fills. Gregg's palette was designed for a
// white page; on a dark one the fills move, and "check, don't assume" means
// computing the ratio rather than eyeballing it. 4.5:1 is WCAG AA for the
// 11px label text, and the hatched figure assumes the worst pixel.
func TestLabelsStayLegibleOnEveryFillInBothThemes(t *testing.T) {
	require.Contains(t, paletteCSS,
		fmt.Sprintf("--frame-ink: hsl(%d %d%% %d%%)", frameInkH, frameInkS, frameInkL),
		"this test's ink must be the ink the page uses")
	ink := hsl{frameInkH, frameInkS, frameInkL}

	for _, theme := range []struct {
		name          string
		dl, ds, alpha float64
	}{
		{"light", lightDL, lightDS, lightHatch},
		{"dark", darkDL, darkDS, darkHatch},
	} {
		for name, c := range palette(t) {
			fill := c.themed(theme.dl, theme.ds)
			assert.GreaterOrEqual(t, contrast(ink, fill), 4.5,
				"%s theme: label ink on %s is %.2f:1", theme.name, name, contrast(ink, fill))
			hatched := overHatch(fill, theme.alpha)
			assert.GreaterOrEqual(t, contrast(ink, hatched), 4.5,
				"%s theme: label ink on hatched %s is %.2f:1", theme.name, name, contrast(ink, hatched))
		}
	}
}

// A label drawn in a colour that flips with the page would be dark ink on a
// dark fill in one theme. The fills stay light in both, so the ink must not
// move at all — and neither must the outlines drawn on top of a fill.
func TestInkAndFrameOutlinesDoNotFollowThePageTheme(t *testing.T) {
	assert.NotContains(t, paletteCSS, "var(--ink)",
		"the page's ink inverts with the page; a frame's does not")
	assert.Contains(t, paletteCSS, "svg.flame text{fill:var(--frame-ink)}")
	assert.Contains(t, paletteCSS, "g.frame:hover rect.bg{stroke:var(--frame-ink)")
	assert.Contains(t, paletteCSS, "g.frame.match rect.bg{stroke:var(--frame-ink)")
}
