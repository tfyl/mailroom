package auth

import "errors"

// LoginError is a sign-in failure with two audiences.
//
// Message is what the sign-in page is allowed to show, and it is written here rather than
// taken from anywhere else. Cause carries the whole of it — the identity provider's own
// words included — and goes to the operator's log. Keeping the two apart is the entire point
// of the type; LoginMessage says why.
type LoginError struct {
	Message string
	Cause   error
}

func (e *LoginError) Error() string { return e.Cause.Error() }
func (e *LoginError) Unwrap() error { return e.Cause }

// LoginMessage returns the wording that may be rendered for a failure from Callback.
//
// The sign-in page is the one page this product serves to somebody with no session, no prior
// interaction and nothing checked: following a link is enough to reach it. It carries the
// deployment's own domain, its own styling and a real sign-in form. Text an outsider chose
// does not belong on it, escaped or not — `error_description` is a query parameter in a URL
// anybody can construct, and reflecting it made the page render arbitrary attacker-chosen
// wording above a genuine login form on a host the reader has reason to trust. That is not
// an injection; it is a lending of credibility, and escaping does nothing about it.
//
// Anything that is not a *LoginError is reduced to one generic sentence on purpose. An error
// added below later, or one surfacing from a library, carries text nobody has read — an
// oauth2 exchange failure, for instance, quotes the token endpoint's response body. Defaults
// should hold for the errors that do not exist yet, so the default here says nothing.
func LoginMessage(err error) string {
	var known *LoginError
	if errors.As(err, &known) && known.Message != "" {
		return known.Message
	}
	return "Sign-in failed. Try again, or use another sign-in method."
}

// misconfigured covers the codes that mean this instance is set up wrongly rather than
// anything the person at the keyboard did. They share one message because telling them apart
// is the operator's job, and the log is where they are told apart.
const misconfigured = "This instance is not set up correctly for its identity provider. " +
	"The details are in the server log."

// oauthErrorMessages maps the error codes an issuer may return to wording of our own.
//
// An allowlist, and it matches on the code alone. Both `error` and `error_description` are
// attacker-chosen in a callback URL anyone can construct, so neither is rendered: an
// unlisted code is as much unvetted free text as the description is, and gets the same
// treatment. The full pair always reaches the log.
//
// Codes are RFC 6749 §4.1.2.1 and OpenID Connect Core §3.1.2.6. The list is what this
// product wants to distinguish rather than every code that exists, which is why an unknown
// one is a supported outcome instead of a gap.
var oauthErrorMessages = map[string]string{
	"access_denied":              "The sign-in was declined at your identity provider.",
	"login_required":             "Your identity provider needs you to sign in again. Start again from the sign-in page.",
	"consent_required":           "Your identity provider needs your consent before it can sign you in.",
	"interaction_required":       "Your identity provider needs another step before it can sign you in. Start again from the sign-in page.",
	"account_selection_required": "Your identity provider needs you to choose an account. Start again from the sign-in page.",
	"server_error":               "Your identity provider reported a problem of its own. Try again shortly.",
	"temporarily_unavailable":    "Your identity provider is temporarily unavailable. Try again shortly.",

	"invalid_request":            misconfigured,
	"invalid_request_uri":        misconfigured,
	"invalid_request_object":     misconfigured,
	"invalid_scope":              misconfigured,
	"unauthorized_client":        misconfigured,
	"unsupported_response_type":  misconfigured,
	"request_not_supported":      misconfigured,
	"request_uri_not_supported":  misconfigured,
	"registration_not_supported": misconfigured,
}

func oauthErrorMessage(code string) string {
	if message, ok := oauthErrorMessages[code]; ok {
		return message
	}
	return "Your identity provider refused the sign-in."
}
