package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// inviteCookie carries a code from the invite link through the sign-in round trip, which may
// leave the site entirely and come back from an identity provider. SameSite=Lax survives
// that, because the return leg is a top-level navigation.
const inviteCookie = "mailroom_invite"

// inviteWindow is how long a code stays in the browser. Long enough to sign in, short enough
// that a code forgotten on a shared machine is not still waiting there tomorrow.
const inviteWindow = 30 * time.Minute

func (s *Server) setInvite(w http.ResponseWriter, code string) {
	http.SetCookie(w, &http.Cookie{
		Name: inviteCookie, Value: code, Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
		MaxAge: int(inviteWindow / time.Second),
	})
}

func (s *Server) clearInvite(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: inviteCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func inviteFrom(r *http.Request) string {
	c, err := r.Cookie(inviteCookie)
	if err != nil {
		return ""
	}
	return signup.NormalizeCode(c.Value)
}

// acceptInvite takes the code out of the link and into a cookie, then sends the visitor to
// sign in.
//
// The code travels this way rather than as a query parameter carried through the login flow
// because it is a credential: a URL ends up in referrer headers, proxy logs and browser
// history, and an identity provider would see it on the way past.
func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	code := signup.NormalizeCode(r.PathValue("code"))
	if code == "" {
		http.Error(w, "that invite link is incomplete", http.StatusBadRequest)
		return
	}
	s.setInvite(w, code)

	// Deliberately no check that the code is valid. Answering that here would let anyone
	// test codes without signing in, and a code that turns out to be spent gives the same
	// refusal as one that never existed.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- Owner-facing management ---

func (s *Server) invites(w http.ResponseWriter, r *http.Request) {
	s.renderInvites(w, r, "", nil)
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	me, owner := s.requireOwner(w, r)
	if !owner {
		return
	}

	ttl := 7 * 24 * time.Hour
	switch r.FormValue("expires") {
	case "24h":
		ttl = 24 * time.Hour
	case "30d":
		ttl = 30 * 24 * time.Hour
	case "never":
		ttl = 0
	}

	_, code, err := s.store.CreateInvite(r.Context(), me.ID, strings.TrimSpace(r.FormValue("note")), ttl)
	if err != nil {
		s.log.Error("creating an invite failed", "err", err)
		http.Error(w, "could not create the invite", http.StatusInternalServerError)
		return
	}

	// Rendered here rather than redirecting, because the code exists only in this response.
	// A redirect would have to carry it in the URL, which is exactly where a credential
	// should not go.
	s.renderInvites(w, r, code, nil)
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	if _, owner := s.requireOwner(w, r); !owner {
		return
	}

	err := s.store.RevokeInvite(r.Context(), r.FormValue("id"))
	if err != nil && !errors.Is(err, store.ErrNoInvite) {
		s.log.Error("revoking an invite failed", "err", err)
		http.Error(w, "could not revoke the invite", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/invites", http.StatusSeeOther)
}

// requireOwner writes the refusal itself and reports whether the caller may continue.
func (s *Server) requireOwner(w http.ResponseWriter, r *http.Request) (user.User, bool) {
	u, signedIn := currentUser(r)
	if !signedIn {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return user.User{}, false
	}
	owner, err := s.store.IsOwner(r.Context(), u.ID)
	if err != nil {
		s.log.Error("checking instance ownership failed", "err", err)
		http.Error(w, "could not check permissions", http.StatusInternalServerError)
		return user.User{}, false
	}
	if !owner {
		http.Error(w, "only the account that set up this instance can manage invites",
			http.StatusForbidden)
		return user.User{}, false
	}
	return u, true
}

func (s *Server) renderInvites(w http.ResponseWriter, r *http.Request, fresh string, _ error) {
	if _, ok := s.requireOwner(w, r); !ok {
		return
	}

	list, err := s.store.ListInvites(r.Context())
	if err != nil {
		s.log.Error("listing invites failed", "err", err)
		http.Error(w, "could not load invites", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	type row struct {
		store.Invite
		State string
	}
	rows := make([]row, 0, len(list))
	for _, inv := range list {
		rows = append(rows, row{Invite: inv, State: inv.State(now)})
	}

	data := map[string]any{
		"Invites":  rows,
		"Policy":   s.signups.Mode,
		"Explain":  s.signups.Describe(),
		"InInvite": s.signups.Mode == signup.Invite,
	}
	if fresh != "" {
		data["NewCode"] = fresh
		data["NewLink"] = s.publicURL + "/invite/" + fresh
	}
	s.render(w, r, "invites", "Invites", "invites", data)
}
