package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tfyl/mailroom/internal/auth"
	"github.com/tfyl/mailroom/internal/signup"
)

// The refusal writes its status before rendering, so the Content-Type is set after
// WriteHeader. That order is safe — Go buffers the header until the first body write — but
// it is worth pinning, and a ResponseRecorder cannot pin it: the recorder keeps accepting
// header writes after WriteHeader, so it would pass either way.
func TestRefusalIsServedAsHTMLOverARealConnection(t *testing.T) {
	s, _ := testServer(t, signup.Policy{Mode: signup.Closed})
	signInAs(s, "ada", "")

	refuse := s.withUser(func(http.ResponseWriter, *http.Request) {})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refuse(w, r.WithContext(auth.WithOperator(r.Context(), auth.Operator{
			Issuer: "https://idp.example.com", Subject: "stranger", Email: "stranger@example.com",
		})))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("want text/html, got %q", ct)
	}
}
