package flamegraph

import (
	"encoding/json"
	"sort"
)

// moduleTable maps each distinct module path on a page to a small index.
//
// The page declares the paths once and every frame carries the index, for the
// same reason a domain carries a class rather than an inline colour: thirteen
// distinct strings repeated across tens of thousands of frames is page weight,
// not information.
type moduleTable struct {
	paths []string
	index map[string]int
}

// internModules collects the distinct modules in the tree.
//
// Sorted, so the same profile renders byte-identical every time. An
// insertion-ordered table would make the page depend on tree-walk order, which
// turns any future reordering into a spurious diff.
func internModules(root *node) moduleTable {
	seen := map[string]struct{}{}
	var walk func(*node)
	walk = func(n *node) {
		if n == nil {
			return
		}
		if n.module != "" {
			seen[n.module] = struct{}{}
		}
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(root)

	t := moduleTable{index: make(map[string]int, len(seen))}
	for m := range seen {
		t.paths = append(t.paths, m)
	}
	sort.Strings(t.paths)
	for i, m := range t.paths {
		t.index[m] = i
	}
	return t
}

// writeModuleTable emits the page's module list.
//
// JSON in a script element rather than a data attribute: a module path may
// contain any character, and JSON is the one encoding a reader can parse back
// with no escaping rules of its own. It is read with JSON.parse on
// textContent, which needs no eval and no relaxation of the page's CSP.
//
// Nothing is emitted when the page has no modules, so a profile without
// mappings costs nothing for a table it would never reference.
func writeModuleTable(ew *errWriter, t moduleTable) {
	if len(t.paths) == 0 {
		return
	}
	b, err := json.Marshal(t.paths)
	if err != nil {
		return
	}
	// NOT html.EscapeString. HTML entities are not decoded inside a script
	// element, so escaping here would hand JSON.parse a document full of
	// &#34; and the table would be unreadable -- a failure that appears only
	// in the browser, long after any Go test has passed.
	//
	// json.Marshal already escapes <, > and & as \u003c, \u003e and \u0026,
	// which is exactly the escaping this context needs: it makes "</script>"
	// unrepresentable in the output, so a module path cannot close the
	// element early.
	ew.f("<script type=\"application/json\" id=\"modules\">%s</script>\n", b)
}

// writeDomainLabels emits the key -> human label map the details panel reads.
//
// The frames carry data-domain="app", which is an identifier chosen for CSS
// selectors, not a word to show a reader. The panel used to print it raw, so a
// frame's domain read "app" and "unsym" instead of "application" and "vendor,
// no symbols". The labels live in Go, in domainInfo, and this is the one hop
// that gets them to the page.
//
// Same escaping reasoning as writeModuleTable above: json.Marshal, not
// html.EscapeString, because entities are not decoded inside a script element.
func writeDomainLabels(ew *errWriter, present map[Domain]bool) {
	// Only the domains this profile actually contains, which is the same rule
	// the legend follows and for the same reason: naming a domain that is not
	// on screen sends the reader looking for it. It is also what
	// TestRenderHTMLLegendOnlyNamesDomainsThatAreActuallyPresent checks, and
	// emitting the whole table quietly broke it.
	m := make(map[string]string, len(present))
	for d := range present {
		in := d.Info()
		if in.Key != "" {
			m[in.Key] = in.Label
		}
	}
	if len(m) == 0 {
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	ew.f("<script type=\"application/json\" id=\"domain-labels\">%s</script>\n", b)
}
