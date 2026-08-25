package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

// googleLinking builds a server with a Google client configured, which is what the linking
// handlers check before they look at anything else.
func googleLinking(t *testing.T) (*Server, user.User, user.User) {
	t.Helper()

	public, err := url.Parse("https://mail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s, db := testServerWith(t, signup.Policy{Mode: signup.Open}, &config.Config{
		PublicURL: public,
		Google:    config.ProviderOAuth{ClientID: "client", ClientSecret: "secret"},
	})

	signInAs(s, "ada", "")
	signInAs(s, "mallory", "")
	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	find := func(subject string) user.User {
		for _, u := range users {
			if u.Subject == subject {
				return u
			}
		}
		t.Fatalf("no user with subject %q", subject)
		return user.User{}
	}
	return s, find("ada"), find("mallory")
}

// startGoogleLink drives the form post and hands back the state Google would be sent.
func startGoogleLink(t *testing.T, s *Server, who user.User, alias string) string {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/accounts/link/google",
		strings.NewReader(url.Values{"alias": {alias}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), who))

	rec := httptest.NewRecorder()
	s.linkGoogle(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect to Google, got %d: %s", rec.Code, rec.Body)
	}
	consent, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := consent.Query().Get("state")
	if state == "" {
		t.Fatal("the redirect to Google carried no state")
	}
	return state
}

func googleCallback(s *Server, who user.User, state string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/accounts/link/google/callback?code=xyzzy&state="+url.QueryEscape(state), nil)
	ctx := user.NewContext(r.Context(), who)
	// Any exchange that gets as far as Google fails here rather than leaving the test
	// dependent on the network — which is also how a callback that passed the ownership
	// check is told apart from one that did not.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: refusingTransport{}})

	rec := httptest.NewRecorder()
	s.linkGoogleCallback(rec, r.WithContext(ctx))
	return rec
}

type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in this test")
}

// The attack this binding exists to stop: someone completes Google's consent for a mailbox
// they own, does not follow the redirect, and gets a signed-in victim to open the callback
// URL instead. It is a top-level GET, so the victim's session cookie rides along, and the
// attacker's mailbox lands in the victim's account under an alias the attacker chose.
func TestGoogleLinkCallbackRefusesTheUserWhoDidNotStartIt(t *testing.T) {
	s, ada, mallory := googleLinking(t)

	state := startGoogleLink(t, s, mallory, "inbox")

	rec := googleCallback(s, ada, state)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a callback belonging to another user must be refused, got %d: %s", rec.Code, rec.Body)
	}
	if _, ok := s.links.take(state, mallory.ID); ok {
		t.Error("the refused attempt is still claimable; a second victim could complete it")
	}
}

// The same request from the user who started it gets past the ownership check, which is
// visible as the next failure along rather than as the same refusal.
func TestGoogleLinkCallbackAcceptsTheUserWhoStartedIt(t *testing.T) {
	s, ada, _ := googleLinking(t)

	state := startGoogleLink(t, s, ada, "work")

	rec := googleCallback(s, ada, state)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("the owner's callback should have reached the token exchange, got %d: %s", rec.Code, rec.Body)
	}
}

// Refusing has to say the same thing whether the state was never issued, has expired, or
// belongs to somebody else. Anything finer tells a caller which states are live.
func TestGoogleLinkCallbackRefusalRevealsNothingAboutTheState(t *testing.T) {
	s, ada, mallory := googleLinking(t)

	somebodyElses := googleCallback(s, ada, startGoogleLink(t, s, mallory, "inbox")).Body.String()
	invented := googleCallback(s, ada, "link_never_issued").Body.String()

	if somebodyElses != invented {
		t.Errorf("the two refusals differ:\n%q\n%q", somebodyElses, invented)
	}
}

func TestLinkStoreHandsAnAttemptBackOnlyToItsOwner(t *testing.T) {
	links := newLinkStore(time.Minute)
	links.put("state", linkAttempt{Owner: "user_ada", Alias: "work"})

	if _, ok := links.take("state", "user_mallory"); ok {
		t.Fatal("another user claimed the attempt")
	}
	if _, ok := links.take("state", "user_ada"); ok {
		t.Fatal("a claimed attempt is still there to be replayed")
	}

	links.put("expired", linkAttempt{Owner: "user_ada", Alias: "work"})
	links.data["expired"] = linkAttempt{Owner: "user_ada", Alias: "work", expires: time.Now().Add(-time.Second)}
	if _, ok := links.take("expired", "user_ada"); ok {
		t.Fatal("an expired attempt was accepted")
	}
}
