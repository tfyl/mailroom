package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/signup"
)

// The script has to arrive, as script, or every enhancement is silently absent. That is a
// survivable failure by design — the pages work without it — which is exactly why it needs a
// test: nothing else about a broken script route would look wrong.
func TestTheScriptIsServed(t *testing.T) {
	rec := httptest.NewRecorder()
	mounted(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, scriptURL, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 from %s, got %d", scriptURL, rec.Code)
	}
	// Every response here carries X-Content-Type-Options: nosniff, so a script served as
	// anything but a JavaScript type is refused by the browser rather than guessed at.
	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("a script served as %q will not be executed: %s", ct, scriptURL)
	}

	body := rec.Body.String()
	if len(body) < 500 {
		t.Fatalf("only %d bytes of script: that is not the file this UI was built against", len(body))
	}
	// The enhancement registry and the one enhancement registered in it. A file that lost
	// either would still be valid JavaScript and would do nothing.
	for _, want := range []string{"data-enhance", "data-js-text", "consent", "reselect"} {
		if !strings.Contains(body, want) {
			t.Errorf("the script is missing %q", want)
		}
	}
	// The rules the policy relies on, asserted against the file rather than against a memory
	// of having followed them. Against the code alone: the comment at the top of the file
	// lists the same names in the course of forbidding them.
	code := withoutComments(body)
	for _, forbidden := range []string{"eval(", "new Function", "innerHTML", "document.write"} {
		if strings.Contains(code, forbidden) {
			t.Errorf("the script uses %q, which either the policy forbids or html/template "+
				"is the only safe way to do", forbidden)
		}
	}
}

// withoutComments drops whole-line // comments. Enough for this file, which has no others,
// and deliberately not a JavaScript parser.
func withoutComments(source string) string {
	var kept []string
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// assertOnlyTheExternalScript checks a rendered page carries the layout's one external script
// tag and nothing inline: no second tag, no body, no on* attribute. The templates are checked
// at the source by TestNoTemplateCarriesInlineScript; this is the same rule applied to what a
// browser actually received, which is where a value interpolated into the page would show up.
func assertOnlyTheExternalScript(t *testing.T, name, body string) {
	t.Helper()

	if got := strings.Count(strings.ToLower(body), "<script"); got != 1 {
		t.Errorf("%s: want the layout's one script tag, got %d", name, got)
	}
	if !strings.Contains(body, `<script src="`+scriptURL+`" defer></script>`) {
		t.Errorf("%s: the only script tag should be the external one", name)
	}
	if loc := regexp.MustCompile(`(?i)\son[a-z]+\s*=`).FindStringIndex(body); loc != nil {
		t.Errorf("%s carries an inline event handler the policy blocks: %s",
			name, body[loc[0]:min(loc[1]+40, len(body))])
	}
}

// The login page links the script as every page does, and it is served to somebody with no
// session. It carries no data and reads none, so there is nothing behind the guard to protect.
func TestTheScriptIsReachableWithoutASession(t *testing.T) {
	srv := httptest.NewServer(mounted(t))
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv.URL + scriptURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an unauthenticated request for the script got %d", resp.StatusCode)
	}
}

// Addressed by its contents, for the reason the stylesheet is: a year-long cache lifetime is
// only safe while a changed file is a changed URL. It matters more here than there — markup
// and the script that enhances it are upgraded together, and a browser holding yesterday's
// script against today's page is a mismatch nobody would think to look for.
func TestTheScriptIsAddressedByItsContents(t *testing.T) {
	if !regexp.MustCompile(`^/static/app\.[0-9a-f]{12}\.js$`).MatchString(scriptURL) {
		t.Fatalf("the script URL should carry a digest, got %s", scriptURL)
	}
	if scriptURL == stylesheetURL {
		t.Fatal("the script and the stylesheet cannot share a URL")
	}

	rec := httptest.NewRecorder()
	mounted(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, scriptURL, nil))

	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("a digest-addressed file should be cacheable indefinitely, got %q", cc)
	}
	if etag := rec.Header().Get("ETag"); !strings.Contains(etag, digest(script)) {
		t.Errorf("ETag %q does not identify the bytes being served", etag)
	}
}

// One script tag, external, deferred, on every page — including the one served before sign-in.
func TestEveryPageLinksTheScript(t *testing.T) {
	s, _ := testServer(t, signup.Policy{Mode: signup.Open})

	rec := httptest.NewRecorder()
	s.loginForm(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `<script src="`+scriptURL+`" defer></script>`) {
		t.Errorf("the layout does not link the script: %s", body)
	}
	if got := strings.Count(body, "<script"); got != 1 {
		t.Errorf("want exactly one script tag on a page, got %d: %s", got, body)
	}
}
