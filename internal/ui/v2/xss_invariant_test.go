package v2

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// The dashboard builds every node through el() (static/app.js), which calls
// document.createElement and assigns text through textContent — it never parses
// a string as HTML. That is the whole XSS defense: capture data is attacker-
// influenced (object names, labels, annotations, log lines, container images all
// come from whatever cluster was captured), and none of it is escaped anywhere,
// because none of it is ever meant to reach an HTML parser.
//
// That invariant is convention, enforced by review. These tests make it
// enforced by CI instead (#263), so a future `innerHTML = ` + template literal
// fails the build rather than silently reintroducing the vector. They are
// deliberately browser-free so they run on every OS in the normal test matrix,
// unlike the route smoke tests behind the `browser` build tag.

// htmlParsingSinks are the APIs that turn a string into DOM. Each one is a way
// to reintroduce the vector, so each is forbidden outright — except .innerHTML,
// which is checked separately below because clearing a container by assigning an
// empty string is both safe and used ~30 times.
var htmlParsingSinks = []struct {
	pattern *regexp.Regexp
	why     string
}{
	{regexp.MustCompile(`\.outerHTML\s*=`), "assigns parsed HTML; build nodes with el() instead"},
	{regexp.MustCompile(`\binsertAdjacentHTML\s*\(`), "parses HTML; use el() + appendChild/insertBefore"},
	{regexp.MustCompile(`\bdocument\s*\.\s*write(ln)?\s*\(`), "parses HTML"},
	{regexp.MustCompile(`\bcreateContextualFragment\s*\(`), "parses HTML"},
	{regexp.MustCompile(`\.srcdoc\s*=`), "embeds a parsed HTML document"},
	{regexp.MustCompile(`\beval\s*\(`), "executes strings as code"},
	{regexp.MustCompile(`\bnew\s+Function\s*\(`), "executes strings as code"},
	// setTimeout/setInterval evaluate a *string* first argument as code. A
	// function argument is fine, so only flag a literal string.
	{regexp.MustCompile(`\bset(Timeout|Interval)\s*\(\s*['"\x60]`), "evaluates a string argument as code; pass a function"},
}

// innerHTMLAssign matches any assignment to .innerHTML, capturing the RHS up to
// the statement end so it can be checked for being an empty-string clear.
var innerHTMLAssign = regexp.MustCompile(`\.innerHTML\s*=\s*([^;\n]*)`)

// emptyStringRHS is the only .innerHTML assignment allowed: clearing a
// container. Accepts an empty single-quoted, double-quoted, or backtick literal,
// with optional surrounding space.
var emptyStringRHS = regexp.MustCompile(`^\s*(''|""|\x60\x60)\s*$`)

func TestStaticAssets_NoHTMLParsingSinks(t *testing.T) {
	for name, src := range readStaticSources(t) {
		stripped := stripJSComments(src)

		for _, sink := range htmlParsingSinks {
			for _, m := range sink.pattern.FindAllString(stripped, -1) {
				t.Errorf("%s: forbidden HTML/code sink %q — %s\n"+
					"    The dashboard renders unescaped capture data, so it must never feed an HTML parser (#263).",
					name, strings.TrimSpace(m), sink.why)
			}
		}

		for _, m := range innerHTMLAssign.FindAllStringSubmatch(stripped, -1) {
			if emptyStringRHS.MatchString(m[1]) {
				continue // clearing a container: safe
			}
			t.Errorf("%s: .innerHTML assigned something other than an empty string: %q\n"+
				"    Only `x.innerHTML = ''` (clearing) is allowed. To add content, build nodes\n"+
				"    with el() and appendChild them — capture data reaching an HTML parser is an\n"+
				"    XSS vector, and nothing in this app escapes it (#263).",
				name, strings.TrimSpace(m[0]))
		}
	}
}

// TestAppJS_ElBuildsNodesSafely pins the specific properties of el() that make
// the sink ban above sufficient. Without these, banning innerHTML would just
// move the vector: an attribute-based one (href="javascript:", onclick="...")
// or a child that smuggles in a <script>.
func TestAppJS_ElBuildsNodesSafely(t *testing.T) {
	src := readStaticSources(t)["app.js"]
	if src == "" {
		t.Fatal("app.js not found in the embedded static FS")
	}
	// Collapse whitespace so these don't break on reflowing.
	flat := regexp.MustCompile(`\s+`).ReplaceAllString(src, " ")

	for _, want := range []struct {
		pattern *regexp.Regexp
		what    string
	}{
		{regexp.MustCompile(`const el = \(tag, attrs = \{\}, \.\.\.children\)`),
			"el() is the node constructor"},
		{regexp.MustCompile(`document\.createElement\(tag\)`),
			"el() creates elements rather than parsing markup"},
		// Both 'text' and the deprecated 'html' alias must go through
		// textContent. 'html' is the trap: a caller reasonably expects it to
		// parse, and it must not.
		{regexp.MustCompile(`k === 'text' \|\| k === 'html'\) n\.textContent =`),
			"el() routes both 'text' and the deprecated 'html' attr through textContent"},
		// Event handlers only from actual functions, never from a string.
		{regexp.MustCompile(`k\.startsWith\('on'\) && typeof v === 'function'`),
			"el() only wires event handlers from functions, never from strings"},
		{regexp.MustCompile(`isSafeChildNode\(c\)`),
			"el() validates element children"},
		// Anything that isn't a validated node is coerced to text, so an
		// unexpected value degrades to visible text instead of markup.
		{regexp.MustCompile(`n\.appendChild\(document\.createTextNode\(String\(c\)\)\)`),
			"el() coerces unvalidated children to text"},
	} {
		if !want.pattern.MatchString(flat) {
			t.Errorf("app.js: lost the invariant that %s\n    (looked for /%s/)", want.what, want.pattern)
		}
	}
}

// TestAppJS_IsSafeChildNodeFailsClosed covers the validator's own discipline:
// it must reject unknown node types, forbidden tags anywhere in the subtree,
// and unsafe URL attributes — and it must fail closed if the walk itself throws.
func TestAppJS_IsSafeChildNodeFailsClosed(t *testing.T) {
	src := readStaticSources(t)["app.js"]
	if src == "" {
		t.Fatal("app.js not found in the embedded static FS")
	}
	flat := regexp.MustCompile(`\s+`).ReplaceAllString(src, " ")

	for _, want := range []struct {
		pattern *regexp.Regexp
		what    string
	}{
		{regexp.MustCompile(`FORBIDDEN_CHILD_TAGS_SET\.has\(tn\)`),
			"forbidden tags are rejected"},
		{regexp.MustCompile(`hasUnsafeAttributes\(`),
			"unsafe attributes are rejected"},
		{regexp.MustCompile(`createTreeWalker\(node, NodeFilter\.SHOW_ELEMENT\)`),
			"the whole subtree is walked, not just the root"},
		{regexp.MustCompile(`catch \(_\) \{ ok = false;`),
			"a throwing walk fails closed"},
		{regexp.MustCompile(`UNSAFE_URL_RE\.test\(normalized\)`),
			"URL attributes are matched after normalization"},
		// Control characters are stripped before the URL test, so
		// "java\nscript:" can't slip past a naive prefix match.
		{regexp.MustCompile(`replace\(/\[\\u0000-\\u0020\\u007f\]\+/g, ''\)`),
			"URL values are stripped of control characters before matching"},
	} {
		if !want.pattern.MatchString(flat) {
			t.Errorf("app.js: lost the invariant that %s\n    (looked for /%s/)", want.what, want.pattern)
		}
	}
}

// TestXSSInvariantTest_CatchesRegressions is a test of the tests. A source
// scanner that silently matches nothing is worse than no scanner, because a
// green run reads as "the invariant holds" (the #334 lesson). This feeds the
// scanner the regressions it exists to catch and asserts each is reported.
func TestXSSInvariantTest_CatchesRegressions(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"innerHTML with a template literal", "host.innerHTML = `<b>${name}</b>`;"},
		{"innerHTML with concatenation", `host.innerHTML = '<b>' + name + '</b>';`},
		{"innerHTML from a variable", "host.innerHTML = rendered;"},
		{"outerHTML", "node.outerHTML = markup;"},
		{"insertAdjacentHTML", `host.insertAdjacentHTML('beforeend', markup);`},
		{"document.write", `document.write(markup);`},
		{"createContextualFragment", `range.createContextualFragment(markup);`},
		{"srcdoc", "frame.srcdoc = doc;"},
		{"eval", `eval(expr);`},
		{"new Function", `const f = new Function('return ' + expr);`},
		{"setTimeout with a string", `setTimeout('doThing()', 10);`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !scanFindsProblem(tc.src) {
				t.Errorf("the scanner did not flag %s:\n    %s\n"+
					"    This scanner is the only automated guard on the XSS invariant; a gap in it\n"+
					"    means a real regression would pass CI.", tc.name, tc.src)
			}
		})
	}

	// And the converse: the forms the app legitimately uses must not be flagged,
	// or the scanner would be permanently red and get deleted.
	for _, ok := range []string{
		`host.innerHTML = '';`,
		`host.innerHTML = "";`,
		"host.innerHTML = ``;",
		`setTimeout(() => tick(), 10);`,
		`setTimeout(tick, 10);`,
		`n.textContent = String(v);`,
		`// innerHTML = markup is forbidden; see the scanner`,
		`/* historical note: this used innerHTML = html */`,
	} {
		if scanFindsProblem(ok) {
			t.Errorf("the scanner flagged a legitimate form, which would make it unusable:\n    %s", ok)
		}
	}
}

// scanFindsProblem applies the same checks as TestStaticAssets_NoHTMLParsingSinks
// to a source fragment, so the scanner and its self-test can't drift apart.
func scanFindsProblem(src string) bool {
	stripped := stripJSComments(src)
	for _, sink := range htmlParsingSinks {
		if sink.pattern.MatchString(stripped) {
			return true
		}
	}
	for _, m := range innerHTMLAssign.FindAllStringSubmatch(stripped, -1) {
		if !emptyStringRHS.MatchString(m[1]) {
			return true
		}
	}
	return false
}

// readStaticSources returns the embedded static assets that can contain script,
// keyed by base name. Reading through staticFS rather than the working tree
// means the tests inspect exactly the bytes //go:embed ships.
func readStaticSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch path.Ext(p) {
		case ".js", ".html":
			b, err := fs.ReadFile(staticFS, p)
			if err != nil {
				return err
			}
			out[path.Base(p)] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded static FS: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no .js/.html found in the embedded static FS — the scan would pass vacuously")
	}
	return out
}

var (
	lineComment  = regexp.MustCompile(`(?m)//.*$`)
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// stripJSComments removes line and block comments so prose describing a sink
// (this file's own guidance ends up quoted in app.js comments) isn't mistaken
// for one. Comments only — string and template literals are left intact.
//
// It is deliberately not a JS parser. Comment stripping is enough for the shapes
// that occur here, and leaving literals in place is the safer direction: a
// payload string that happens to contain "innerHTML =" gets reported rather than
// hidden. A scanner that over-reports fails loudly and gets looked at; one that
// under-reports fails silently. The self-test above pins both directions.
func stripJSComments(src string) string {
	src = blockComment.ReplaceAllString(src, " ")
	return lineComment.ReplaceAllString(src, "")
}
