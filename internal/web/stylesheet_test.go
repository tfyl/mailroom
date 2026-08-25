package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/oauthsrv"
	"github.com/tfyl/mailroom/internal/signup"
)

// mounted builds the whole browser surface the way main does, so the tests below exercise the
// stylesheet through the mux rather than by calling its handler directly. Where a route is
// registered is half of what is being asserted here.
func mounted(t *testing.T) http.Handler {
	t.Helper()
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	mux := http.NewServeMux()
	s.Routes(mux, oauthsrv.New(db, "https://mail.example.com"))
	return SecurityHeaders(nil, mux)
}

// The whole point of moving the CSS out of the document: it has to arrive, as CSS, or every
// page is unstyled markup and the tightened policy bought nothing.
func TestTheStylesheetIsServed(t *testing.T) {
	rec := httptest.NewRecorder()
	mounted(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, stylesheetURL, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 from %s, got %d", stylesheetURL, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("a stylesheet served as %q is ignored by the browser", ct)
	}

	body := rec.Body.String()
	if len(body) < 10_000 {
		t.Fatalf("only %d bytes of CSS: the build produced nothing much", len(body))
	}
	// Named markers rather than a size alone, because an empty Tailwind build is still a
	// valid stylesheet several kilobytes long. These three are the layers this design system
	// is made of: Tailwind's own output, this design system's tokens, and Basecoat's components.
	for _, want := range []string{"tailwindcss", "--primary:", "--success:", ".btn", ".card", ".badge"} {
		if !strings.Contains(body, want) {
			t.Errorf("the stylesheet is missing %q, so it is not the one this UI was built against", want)
		}
	}
	// Both themes ship in the one file and the browser picks. A build that lost the media
	// query would leave every dark-mode user on the light palette.
	if !strings.Contains(body, "prefers-color-scheme:dark") {
		t.Error("no dark palette in the stylesheet")
	}
}

// The sign-in page is served to somebody with no session, so the stylesheet it asks for has
// to be reachable without one. Getting this wrong leaves exactly one page unstyled, and it is
// the first page anybody sees.
func TestTheStylesheetIsReachableWithoutASession(t *testing.T) {
	srv := httptest.NewServer(mounted(t))
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv.URL + stylesheetURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an unauthenticated request for the stylesheet got %d", resp.StatusCode)
	}
}

// The name carries a digest of the contents, which is what makes a year-long cache lifetime
// safe: a changed stylesheet is a changed URL, so nothing cached under the old one is ever
// served for the new one.
func TestTheStylesheetIsAddressedByItsContents(t *testing.T) {
	if !regexp.MustCompile(`^/static/app\.[0-9a-f]{12}\.css$`).MatchString(stylesheetURL) {
		t.Fatalf("the stylesheet URL should carry a digest, got %s", stylesheetURL)
	}

	rec := httptest.NewRecorder()
	mounted(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, stylesheetURL, nil))

	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("a digest-addressed file should be cacheable indefinitely, got %q", cc)
	}
	if etag := rec.Header().Get("ETag"); !strings.Contains(etag, digest(stylesheet)) {
		t.Errorf("ETag %q does not identify the bytes being served", etag)
	}
}

// Every page draws its own styling from the same file. A page rendered without the link is a
// page that arrives unstyled, and there is no second stylesheet to fall back to.
func TestEveryPageLinksTheStylesheet(t *testing.T) {
	s, _ := testServer(t, signup.Policy{Mode: signup.Open})

	rec := httptest.NewRecorder()
	s.loginForm(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	if !strings.Contains(rec.Body.String(), `<link rel="stylesheet" href="`+stylesheetURL+`">`) {
		t.Errorf("the layout does not link the stylesheet: %s", rec.Body)
	}
}

// The companion to TestNoTemplateReliesOnScript, and the reason style-src can be 'self': a
// single style attribute anywhere in the templates would need 'unsafe-inline' back, and
// 'unsafe-inline' on style-src is what lets an injection restyle a consent screen.
func TestNoTemplateCarriesAnInlineStyle(t *testing.T) {
	attribute := regexp.MustCompile(`(?i)\sstyle\s*=\s*["']`)

	entries, err := fs.Glob(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no templates found to check")
	}
	for _, name := range entries {
		body, err := fs.ReadFile(files, name)
		if err != nil {
			t.Fatal(err)
		}
		if loc := attribute.FindIndex(body); loc != nil {
			t.Errorf("%s carries a style attribute the policy blocks: %s",
				name, body[loc[0]:min(loc[1]+40, len(body))])
		}
	}
}
