package blob

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/user"
)

// SigningPurpose is the HKDF label the URL-signing key is derived under. It names a version
// because changing the string is how the key is retired: every URL signed under the old one
// stops verifying at once.
const SigningPurpose = "mailroom attachment url signing v1"

// Use separates the two kinds of URL. It is signed along with everything else, so a download
// link can never be replayed as permission to write and an upload URL can never be replayed
// as permission to read.
type Use string

const (
	UseDownload Use = "d"
	UseUpload   Use = "u"
)

var (
	ErrMalformed = errors.New("this link is not in the right shape")
	ErrSignature = errors.New("this link's signature does not verify")
	ErrExpired   = errors.New("this link has expired")
)

// Claims are what a signed URL asserts.
//
// Every field is covered by the signature, and the set is chosen so that a leaked URL is
// narrow in every dimension that matters: it names one blob, on behalf of one owner, under
// one grant, until one moment, and — for an upload — up to one size. Binding the owner and
// the grant is what makes the fetch a scoped lookup rather than a bare id lookup, so a valid
// signature for somebody else's blob resolves to nothing at all.
//
// The declared content type is deliberately not here. It does not need to be: the server
// wrote the type into the blob's row when it minted the URL, and the routes never take a type
// from a request, so there is nothing at PUT time for a caller to influence.
type Claims struct {
	Use     Use
	BlobID  string
	Owner   user.ID
	Grant   grant.ID
	Expires time.Time
	// Max is the byte ceiling an upload URL carries. Zero on a download link.
	Max int64
}

// Signer mints and verifies the tokens that stand in for a session on these routes.
//
// HMAC-SHA256 over a text payload, with the key derived from MAILROOM_ENCRYPTION_KEY rather
// than being that key: see secrets.Derive for why the two uses are kept apart.
type Signer struct{ key []byte }

func NewSigner(key []byte) (*Signer, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("a URL signing key must be at least 32 bytes, got %d", len(key))
	}
	return &Signer{key: key}, nil
}

// payload is the exact string the signature covers.
//
// Dot-separated and unencoded because every field is already URL-safe and dot-free: the ids
// come from internal/ids, whose alphabet is base32 digits and capitals, and the rest are a
// single letter and two integers. Parsing asserts the field count, so no field can absorb
// another's content — which is the failure this shape has to be checked against, not
// prettiness.
func (c Claims) payload() string {
	return strings.Join([]string{
		"v1", string(c.Use), c.BlobID, string(c.Owner), string(c.Grant),
		strconv.FormatInt(c.Expires.Unix(), 10), strconv.FormatInt(c.Max, 10),
	}, ".")
}

func (s *Signer) mac(payload string) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

func (s *Signer) Token(c Claims) string {
	p := c.payload()
	return p + "." + base64.RawURLEncoding.EncodeToString(s.mac(p))
}

// Parse verifies a token and returns what it asserts.
//
// The signature is checked before anything else is believed, and before expiry: a forged
// token must fail as a forgery whatever it claims, and reporting "expired" for one would tell
// an attacker their payload parsed. Comparison is constant time.
func (s *Signer) Parse(token string, now time.Time) (Claims, error) {
	cut := strings.LastIndex(token, ".")
	if cut <= 0 {
		return Claims{}, ErrMalformed
	}
	payload, encoded := token[:cut], token[cut+1:]

	sig, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	if !hmac.Equal(sig, s.mac(payload)) {
		return Claims{}, ErrSignature
	}

	parts := strings.Split(payload, ".")
	if len(parts) != 7 || parts[0] != "v1" {
		return Claims{}, ErrMalformed
	}
	expires, err := strconv.ParseInt(parts[5], 10, 64)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	max, err := strconv.ParseInt(parts[6], 10, 64)
	if err != nil {
		return Claims{}, ErrMalformed
	}

	c := Claims{
		Use:     Use(parts[1]),
		BlobID:  parts[2],
		Owner:   user.ID(parts[3]),
		Grant:   grant.ID(parts[4]),
		Expires: time.Unix(expires, 0).UTC(),
		Max:     max,
	}
	if c.Use != UseDownload && c.Use != UseUpload {
		return Claims{}, ErrMalformed
	}
	if c.BlobID == "" || c.Owner == "" {
		return Claims{}, ErrMalformed
	}
	if !now.Before(c.Expires) {
		return Claims{}, ErrExpired
	}
	return c, nil
}
