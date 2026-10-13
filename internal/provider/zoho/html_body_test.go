package zoho

import (
	"context"
	"net/http"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Zoho's content endpoint does not say what it returned. A message composed in a web client
// comes back as a bare fragment with no <html> around it, and reading that as text put markup
// in the body and lost every link in it.
func TestZohoContentIsFiledAsHTMLWhenItIsMarkup(t *testing.T) {
	for content, wantHTML := range map[string]bool{
		`<html><body><a href="https://example.com/f">Form</a></body></html>`:        true,
		`<div dir="ltr">Fill in the <a href="https://example.com/f">form</a></div>`: true,
		"Fill in the form at https://example.com/f":                                 false,
	} {
		p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/details"):
				writeEnvelope(t, w, map[string]any{"messageId": "m1", "folderId": "f1", "subject": "s"})
			case strings.HasSuffix(r.URL.Path, "/content"):
				writeEnvelope(t, w, map[string]any{"content": content})
			default:
				t.Errorf("unexpected request %s", r.URL.Path)
			}
		})
		msg, err := p.Get(context.Background(), mmail.ScopedID{Account: "acct_1", Native: "f1/m1"})
		if err != nil {
			t.Fatal(err)
		}
		if gotHTML := msg.Body.HTML != ""; gotHTML != wantHTML {
			t.Errorf("%q: filed as html = %v, want %v (body %+v)", content, gotHTML, wantHTML, msg.Body)
		}
	}
}
