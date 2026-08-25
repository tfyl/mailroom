package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The text an attacker would choose. Long enough to be a sentence somebody might act on,
// which is the whole point of the primitive: not script, just words on a trusted host.
const plantedText = "Your mailbox is suspended. Call IT support on 555-0100 to restore it."

func callbackWith(t *testing.T, o *OIDC, query url.Values) error {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/auth/test/callback?"+query.Encode(), nil)
	_, err := o.Callback(httptest.NewRecorder(), r)
	if err == nil {
		t.Fatal("the callback was accepted")
	}
	return err
}

// The defect: error_description came straight back out of the URL and onto the sign-in page.
//
// It is not an injection — html/template escapes it — and calling it one misreads what is
// wrong. The page is served from the operator's own domain, styled as their own, with a real
// sign-in form on it, and a bare link is enough to reach it. Whatever the attacker writes
// appears above that form as though the deployment said it.
func TestTheIssuersErrorDescriptionNeverBecomesAMessage(t *testing.T) {
	const state = "state-value"
	o := &OIDC{pending: newPendingLogins(time.Minute)}
	o.pending.put(state, pendingLogin{Next: "/", Verifier: "v", Nonce: "n", Binder: "b"})

	err := callbackWith(t, o, url.Values{
		"state":             {state},
		"error":             {"access_denied"},
		"error_description": {plantedText},
	})

	if message := LoginMessage(err); strings.Contains(message, "555-0100") ||
		strings.Contains(message, "IT support") || strings.Contains(message, "suspended") {
		t.Fatalf("the provider's text became the message shown to a person: %q", message)
	}

	// It is still worth having, so it has to be somewhere: the operator's log, which is what
	// Error() feeds.
	if !strings.Contains(err.Error(), plantedText) {
		t.Fatalf("the provider's text was dropped instead of logged: %q", err.Error())
	}
}

// An unlisted code is free text exactly as much as the description is, and an attacker who
// finds the description filtered will simply move their sentence one parameter along.
func TestAnUnrecognisedErrorCodeIsNotEchoedEither(t *testing.T) {
	const state = "state-value"
	o := &OIDC{pending: newPendingLogins(time.Minute)}
	o.pending.put(state, pendingLogin{Next: "/", Verifier: "v", Nonce: "n", Binder: "b"})

	err := callbackWith(t, o, url.Values{
		"state": {state},
		"error": {"your_mailbox_is_suspended_call_555_0100"},
	})

	if message := LoginMessage(err); strings.Contains(message, "555") ||
		strings.Contains(message, "suspended") || strings.Contains(message, "mailbox") {
		t.Fatalf("the error code became the message shown to a person: %q", message)
	}
	if !strings.Contains(err.Error(), "your_mailbox_is_suspended_call_555_0100") {
		t.Fatalf("the code was dropped instead of logged: %q", err.Error())
	}
}

// The allowlist has to earn its place: filtering everything down to one sentence would be
// safe and useless, and an operator whose users are being refused needs to know it is a
// refusal rather than an outage.
func TestARealRefusalStillSaysWhatHappened(t *testing.T) {
	for code, want := range map[string]string{
		"access_denied":           "declined",
		"temporarily_unavailable": "temporarily unavailable",
		"invalid_scope":           "not set up correctly",
	} {
		t.Run(code, func(t *testing.T) {
			const state = "state-value"
			o := &OIDC{pending: newPendingLogins(time.Minute)}
			o.pending.put(state, pendingLogin{Next: "/", Verifier: "v", Nonce: "n", Binder: "b"})

			err := callbackWith(t, o, url.Values{"state": {state}, "error": {code}})
			if message := LoginMessage(err); !strings.Contains(message, want) {
				t.Fatalf("%s produced %q, which does not say %q", code, message, want)
			}
		})
	}
}

// The other half of the defect: the error was read before the state was, so a link needed no
// relationship to any sign-in this server had started. Checking the state first does not fix
// the reflection on its own — an attacker can start a login of their own to mint a state —
// but it means the ordinary bare link never reaches the branch at all.
func TestABareErrorLinkIsRefusedBeforeTheErrorIsRead(t *testing.T) {
	o := &OIDC{pending: newPendingLogins(time.Minute)}

	err := callbackWith(t, o, url.Values{
		"error":             {"access_denied"},
		"error_description": {plantedText},
	})

	message := LoginMessage(err)
	if strings.Contains(message, "555-0100") || strings.Contains(message, "suspended") {
		t.Fatalf("the provider's text became the message shown to a person: %q", message)
	}
	if !strings.Contains(message, "no longer valid") {
		t.Fatalf("a link matching no sign-in should say so, got %q", message)
	}
	// An issuer that omits the state on an error response is out of spec but not impossible,
	// and the operator still has to be able to tell what happened.
	if !strings.Contains(err.Error(), plantedText) {
		t.Fatalf("nothing was left for the log: %q", err.Error())
	}
}

// The default has to hold for errors nobody has thought about yet. An oauth2 exchange
// failure quotes the token endpoint's response body, which is not text this project has read.
func TestAnUnvettedErrorGetsTheGenericMessage(t *testing.T) {
	for _, err := range []error{
		errors.New("oauth2: cannot fetch token: 400 Bad Request Response: " + plantedText),
		errors.New("verifying id_token: " + plantedText),
	} {
		if message := LoginMessage(err); strings.Contains(message, "555-0100") ||
			strings.Contains(message, "suspended") {
			t.Fatalf("an unvetted error reached the page as %q", message)
		}
	}
}

// Wrapping must not hide a message: LoginError is compared with errors.As so that a failure
// keeps its wording through a fmt.Errorf("%w") somewhere above it.
func TestAWrappedLoginErrorKeepsItsMessage(t *testing.T) {
	inner := &LoginError{Message: "The sign-in was declined.", Cause: errors.New("access_denied")}
	if got := LoginMessage(errors.Join(errors.New("context"), inner)); got != inner.Message {
		t.Fatalf("LoginMessage = %q, want %q", got, inner.Message)
	}
}
