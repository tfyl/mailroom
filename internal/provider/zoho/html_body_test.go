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

// The thread listing carries no content, so every message but the one asked for came back
// with no body and no links.
func TestEveryMessageInAZohoThreadHasItsBody(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/details"):
			writeEnvelope(t, w, map[string]any{"messageId": "m1", "folderId": "f1", "threadId": "m1", "subject": "s"})
		case strings.HasSuffix(r.URL.Path, "/messages/view"):
			writeEnvelope(t, w, []map[string]any{
				{"messageId": "m1", "folderId": "f1", "threadId": "m1", "subject": "s"},
				{"messageId": "m2", "folderId": "f1", "threadId": "m1", "subject": "Re: s"},
			})
		case strings.HasSuffix(r.URL.Path, "/content"):
			writeEnvelope(t, w, map[string]any{"content": `<div><a href="https://example.com` + r.URL.Path + `">x</a></div>`})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	thread, err := p.GetThread(context.Background(), mmail.ScopedID{Account: "acct_1", Native: "f1/m1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 2 {
		t.Fatalf("thread has %d messages", len(thread.Messages))
	}
	for _, m := range thread.Messages {
		if !strings.Contains(m.Body.HTML, "/messages/"+strings.SplitN(m.ID.Native, "/", 2)[1]+"/content") {
			t.Errorf("%s has the wrong body or none: %+v", m.ID, m.Body)
		}
	}
}
