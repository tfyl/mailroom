package gmail

import (
	"testing"

	"github.com/tfyl/mailroom/internal/provider/conformance"
)

// The static half of the contract needs no credentials, so it runs in CI on every change.
// It catches the drift that matters most: a Capabilities set that no longer matches the
// interfaces the type actually implements.
func TestGmailStaticConformance(t *testing.T) {
	conformance.Static(t, &Provider{})
}
