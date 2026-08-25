package web

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// copyrightLines pulls every "Copyright (c) …" line out of a vendored licence. The tests
// below assert against what those files say rather than against a name written here, so a
// re-vendored dependency that changed its holder or its year fails a test instead of quietly
// leaving a shipped notice describing somebody else's release.
var copyrightLines = regexp.MustCompile(`(?m)^Copyright \(c\) .+$`)

// Basecoat is MIT, and MIT asks for its copyright and permission notice to accompany every
// copy of the software. static/app.css is a copy: it is the file this package embeds and the
// only stylesheet a browser ever receives, and minifying the vendored sources into it leaves
// none of their comments behind. `make css` puts the notice back; this is what notices its
// absence.
//
// `make css-check` asserts the same thing and is the more direct check, but it downloads a
// Tailwind binary to do it. This one runs on a clone with nothing but Go, which is the only
// toolchain the project asks a contributor for.
func TestTheStylesheetCarriesTheBasecoatNotice(t *testing.T) {
	vendored := filepath.Join("assets", "basecoat")

	licence, err := os.ReadFile(filepath.Join(vendored, "LICENSE.md"))
	if err != nil {
		t.Fatal(err)
	}
	copyright := copyrightLines.Find(licence)
	if copyright == nil {
		t.Fatalf("no copyright line in %s/LICENSE.md, so there is nothing to require of the stylesheet", vendored)
	}

	version, err := os.ReadFile(filepath.Join(vendored, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	release := regexp.MustCompile(`basecoat-css ([^,\s]+)`).FindSubmatch(version)
	if release == nil {
		t.Fatalf("%s/VERSION names no basecoat-css release: %q", vendored, version)
	}

	css := string(stylesheet)
	for _, want := range []string{"Basecoat", string(release[1]), strings.TrimSpace(string(copyright))} {
		if !strings.Contains(css, want) {
			t.Errorf("the served stylesheet does not carry %q, and MIT asks for the notice to travel with the copy. Run: make css", want)
		}
	}
}

// The two alert marks are Lucide's, and ISC asks for its copyright and permission notice to
// appear in all copies. An inline icon is copied into the markup of every page that draws it,
// so the notice sits in the template beside the paths rather than only in the vendored
// licence text. Both icons are also on Lucide's list of ones derived from Feather, which is
// why there are two holders to name and not one.
func TestTheInlineIconsCarryTheirNotices(t *testing.T) {
	licence, err := os.ReadFile(filepath.Join("assets", "lucide", "LICENSE.md"))
	if err != nil {
		t.Fatal(err)
	}
	notices := copyrightLines.FindAll(licence, -1)
	if len(notices) == 0 {
		t.Fatal("no copyright line in assets/lucide/LICENSE.md, so there is nothing to require of the markup")
	}

	layout, err := fs.ReadFile(files, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range notices {
		if !bytes.Contains(layout, bytes.TrimSpace(want)) {
			t.Errorf("templates/layout.html does not carry %q, so a page that draws an icon ships it without its notice", want)
		}
	}
}
