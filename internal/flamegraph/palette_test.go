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
	// The dark theme is reachable two ways — the reader's OS setting and the
	// D key — so it is declared twice, from one Go constant. Both blocks are
	// parsed and both are held to the same rule.
	darkBlockRe = regexp.MustCompile(`(?s)@media \(prefers-color-scheme: dark\)\{:root:not\(\[data-theme="light"\]\)\{([^}]*)\}\}`)
	darkThemeRe = regexp.MustCompile(`(?s):root\[data-theme="dark"\]\{([^}]*)\}`)
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
	return fromRGB(mix(r, 22.0/255), mix(g, 20.0/255), mix(b, 18.0/255))
}

// lightened is the jitter, as the browser computes it:
// hsl(from fill h s calc(l + N)). Hue and saturation do not move, so a
// jittered frame is the same colour at a different lightness — and N is a
// ladder step, never negative. See the jitter tests below for why that
// direction is forced.
func lightened(c hsl, delta float64) hsl { return hsl{c.h, c.s, c.l + delta} }

// fromRGB converts back to the hsl the rest of this file speaks. Mixing in
// sRGB and re-deriving lightness is what a browser draws; contrast() only
// reads luminance, so this is a luminance-preserving round trip.
func fromRGB(rr, gg, bb float64) hsl {
	m := hsl{}
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
		sel := fmt.Sprintf(`[data-domain="%s"]{--fill-x:%s`, info.Key, info.Fill)
		assert.Contains(t, paletteCSS, sel, "no rule paints domain %q", info.Key)
	}
}

// Aqua is Gregg's accelerator-source layer. perf-agent cannot produce those
// frames yet; the hue is held open rather than spent on something else, and
// the page says so instead of leaving a silent gap in the palette.
func TestAquaIsReservedAndUnused(t *testing.T) {
	assert.Contains(t, palette(t), "fill-accel-source", "the reserved aqua token must exist")
	assert.NotContains(t, paletteCSS, "]{--fill-x:var(--fill-accel-source)",
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
	require.Len(t, m, 2, "paletteCSS must have exactly one dark-theme media block")
	d := darkThemeRe.FindStringSubmatch(paletteCSS)
	require.Len(t, d, 2, "paletteCSS must have exactly one [data-theme=dark] block for the D key")
	assert.Equal(t, m[1], d[1],
		"the OS dark theme and the D key must apply the same declarations, or the page has two dark themes")

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

// The D key must be able to overrule the OS in both directions: a reader on
// a dark desktop who wants the light palette gets it from the same token set.
func TestTheDarkKeyCanOverrideTheOSInBothDirections(t *testing.T) {
	assert.Contains(t, paletteCSS, `:root:not([data-theme="light"])`,
		"[data-theme=light] must be able to win against prefers-color-scheme: dark")
	assert.Contains(t, paletteCSS, `:root[data-theme="dark"]`,
		"[data-theme=dark] must be able to win against a light OS")
	assert.Contains(t, styleSheet, `:root:not([data-theme="light"])`)
	assert.Contains(t, styleSheet, `:root[data-theme="dark"]`)
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
	assert.Contains(t, paletteCSS, `[data-domain="boundary-unattributed"]{--fill-x:var(--fill-boundary-unattributed);background-image:var(--hatch-gap);box-shadow:none;outline:1px dashed var(--edge-unattributed);outline-offset:-1px}`,
		"and dashed, because there is nothing behind it")
	assert.NotContains(t, paletteCSS, `[data-domain="boundary"]{--fill-x:var(--fill-boundary);box-shadow:inset 0 0 0 1px var(--edge-boundary);outline`)
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
			// Every fill is checked at every rung of the jitter ladder, not
			// just at its declared value: a frame on the page is a jittered
			// fill, and an untested rung is an untested colour.
			for step := range jitterSteps {
				base := c.themed(theme.dl, theme.ds)
				fill := lightened(base, jitterLightness(step))
				assert.GreaterOrEqual(t, contrast(ink, fill), 4.5,
					"%s theme: label ink on %s at jitter step %d is %.2f:1", theme.name, name, step, contrast(ink, fill))
				hatched := overHatch(fill, theme.alpha)
				assert.GreaterOrEqual(t, contrast(ink, hatched), 4.5,
					"%s theme: label ink on hatched %s at jitter step %d is %.2f:1", theme.name, name, step, contrast(ink, hatched))
			}
		}
	}
}

// The jitter's direction is not a taste. The darkest thing a label is ever
// drawn on clears 4.5:1 by about a tenth, so there is no room to darken a
// fill and plenty to lighten one — and mixing white in raises luminance
// monotonically, which makes the un-jittered fill the worst case and leaves
// the floor exactly where the test above found it.
func TestJitterOnlyEverLightensSoTheContrastFloorCannotMove(t *testing.T) {
	ink := hsl{frameInkH, frameInkS, frameInkL}

	worstUnjittered, worstAny := math.Inf(1), math.Inf(1)
	for _, theme := range []struct{ dl, ds, alpha float64 }{{lightDL, lightDS, lightHatch}, {darkDL, darkDS, darkHatch}} {
		for _, c := range palette(t) {
			base := c.themed(theme.dl, theme.ds)
			prev := math.Inf(-1)
			for step := range jitterSteps {
				require.GreaterOrEqual(t, jitterLightness(step), 0.0, "a rung of the ladder darkens a fill")
				fill := lightened(base, jitterLightness(step))
				require.LessOrEqual(t, fill.l, 100.0, "a rung of the ladder clips at white")
				got := contrast(ink, overHatch(fill, theme.alpha))
				assert.Greater(t, got, prev, "each rung must be lighter than the last, or the ladder is not monotone")
				prev = got
				worstAny = math.Min(worstAny, got)
				if step == 0 {
					worstUnjittered = math.Min(worstUnjittered, got)
				}
			}
		}
	}
	assert.Equal(t, worstUnjittered, worstAny, "jitter must not create a worse case than the palette already had")
	assert.GreaterOrEqual(t, worstAny, 4.5)
	assert.Less(t, worstAny, 4.7, "if the floor has grown this much headroom, the jitter could be bolder")
}

// The ladder in the stylesheet is the ladder the renderer assigns steps on.
// A mismatch would put a frame on a rung that has no rule and paint it with
// the default 0%, silently un-jittering it.
func TestTheJitterLadderInTheStylesheetMatchesTheRenderer(t *testing.T) {
	assert.Contains(t, paletteCSS, "--fill-j:0;", "the ladder needs a zero rung at :root")
	assert.Contains(t, paletteCSS, "background-color:var(--fill-x);background-color:hsl(from var(--fill-x) h s calc(l + var(--fill-j)))",
		"the fill must be composed on the frame (var() inside a :root custom property freezes at :root), and the plain fill must precede it so an engine without relative colour syntax falls back to the palette rather than to nothing")
	assert.NotContains(t, jitterCSS, ".j0{", "step 0 is the palette's own colour and needs no rule")
	for step := 1; step < jitterSteps; step++ {
		assert.Contains(t, jitterCSS, fmt.Sprintf(".j%d{--fill-j:%.2f}", step, jitterLightness(step)))
	}
	assert.InDelta(t, jitterMaxL, jitterLightness(jitterSteps-1), 0.001, "the top rung is the stated amplitude")
}

// A label drawn in a colour that flips with the page would be dark ink on a
// dark fill in one theme. The fills stay light in both, so the ink must not
// move at all — and neither must the outlines drawn on top of a fill.
func TestInkAndFrameOutlinesDoNotFollowThePageTheme(t *testing.T) {
	assert.NotContains(t, paletteCSS, "var(--ink)",
		"the page's ink inverts with the page; a frame's does not")
	assert.Contains(t, paletteCSS, ".frame{color:var(--frame-ink)")
	assert.Contains(t, paletteCSS, ".frame:hover{box-shadow:inset 0 0 0 1.4px var(--frame-ink)")
	assert.Contains(t, paletteCSS, ".frame.cur{box-shadow:inset 0 0 0 1.8px var(--frame-ink)")
}

// A hatch used to be an SVG <pattern> painted by a second <rect> per frame.
// It is a background-image now, so the honesty signal it carries — "the
// profile could not fully account for this frame" — has to be reattached to
// every domain that claims it, by a rule rather than by an extra element.
func TestEveryHatchedDomainIsActuallyHatchedByARule(t *testing.T) {
	for d := Domain(0); d < numDomains; d++ {
		info := d.Info()
		if info.Overlay == "" {
			continue
		}
		assert.Contains(t, paletteCSS, fmt.Sprintf(`[data-domain="%s"]{--fill-x:%s;background-image:%s`, info.Key, info.Fill, info.Overlay),
			"domain %q says it is hatched but no rule hatches it", info.Key)
		assert.Contains(t, paletteCSS, strings.TrimSuffix(strings.TrimPrefix(info.Overlay, "var("), ")")+":",
			"domain %q names a hatch token the stylesheet does not declare", info.Key)
	}
	// A frame can be both unnamed and inferred; it must then show both
	// hatches rather than silently losing one to the cascade.
	assert.Contains(t, paletteCSS, `.frame.inexact[data-domain="unsym"],.frame.inexact[data-domain="boundary-unattributed"]{background-image:var(--hatch-gap),var(--hatch-inf)}`)
	// The two hatches run opposite ways, so which uncertainty a frame
	// carries is visible without hovering it.
	assert.Contains(t, paletteCSS, "--hatch-gap: repeating-linear-gradient(45deg,")
	assert.Contains(t, paletteCSS, "--hatch-inf: repeating-linear-gradient(-45deg,")
}

// A border would be laid out; a 3px-wide frame with a 1px border on each
// side would have no content box left. Every outline on a frame is painted
// instead — an inset box-shadow, or an outline pulled inside by a negative
// offset — so a sliver stays the width its value earned.
func TestFrameOutlinesAreNeverLaidOut(t *testing.T) {
	for _, decl := range strings.Split(paletteCSS, "\n") {
		if !strings.HasPrefix(decl, ".frame") {
			continue
		}
		assert.NotContains(t, decl, "border:", "a frame outline must not take width from the frame: %s", decl)
		assert.NotContains(t, decl, "border-width", decl)
	}
	assert.Contains(t, paletteCSS, "outline:1px dashed var(--edge-unattributed);outline-offset:-1px",
		"the one dashed outline needs outline, which box-shadow cannot do — but it still must not be laid out")
}

// Magenta is the only colour on the page that does not mean "domain", and it
// has to clear the same floor as the ones that do: a matched frame still has
// a label on it, and a matched frame that is unsymbolized still has a hatch
// over it. It is deliberately outside the themed --fill-* family — it is a
// signal, not a layer, so it must not shift when the page does — which means
// the light theme's heavier hatch is the binding case for both themes.
func TestTheSearchColourIsLegibleAndReservedToSearch(t *testing.T) {
	const matchH, matchS, matchL = 300, 100, 71
	require.Contains(t, paletteCSS,
		fmt.Sprintf("--fill-match: hsl(%d %d%% %d%%)", matchH, matchS, matchL),
		"this test's magenta must be the magenta the page uses")

	ink := hsl{frameInkH, frameInkS, frameInkL}
	match := hsl{matchH, matchS, matchL}
	assert.GreaterOrEqual(t, contrast(ink, match), 4.5,
		"label ink on the search fill is %.2f:1", contrast(ink, match))
	for _, alpha := range []float64{lightHatch, darkHatch} {
		got := contrast(ink, overHatch(match, alpha))
		assert.GreaterOrEqual(t, got, 4.5,
			"label ink on a hatched search fill at hatch alpha %.2f is %.2f:1", alpha, got)
	}

	// It does not move with the theme, and no domain may spend it.
	assert.NotContains(t, paletteCSS, "--fill-match: hsl(300 calc(",
		"the search colour is a signal, not a palette member; the theme knobs must not touch it")
	m := darkBlockRe.FindStringSubmatch(paletteCSS)
	require.Len(t, m, 2)
	assert.NotContains(t, m[1], "--fill-match")
	assert.NotContains(t, paletteCSS, "]{--fill-x:var(--fill-match)")
	for d := Domain(0); d < numDomains; d++ {
		assert.NotEqual(t, "var(--fill-match)", d.Info().Fill, "domain %q took the search colour", d.Info().Key)
	}
	// Search paints the fill; only the one match N is standing on is outlined.
	assert.Contains(t, paletteCSS, ".frame.match{background-color:var(--fill-match)}")
	assert.NotContains(t, paletteCSS, ".frame.match{box-shadow")
}

// A domain is a domain wherever it is drawn. The tree view's swatches carry
// data-domain and take their colour, their hatch and their outline from the
// same rules the frames do, so a domain cannot come out one colour in the
// graph and another in the tree.
func TestDomainRulesAreNotFrameOnly(t *testing.T) {
	for d := Domain(0); d < numDomains; d++ {
		info := d.Info()
		assert.NotContains(t, paletteCSS, fmt.Sprintf(`.frame[data-domain="%s"]{--fill-x:`, info.Key),
			"domain %q paints only frames, so a tree swatch would have no colour", info.Key)
	}
	assert.Contains(t, paletteCSS, ".sw[data-domain]{background-color:var(--fill-x)}")
}
