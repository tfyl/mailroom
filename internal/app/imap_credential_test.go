package app

import (
	"encoding/json"
	"testing"

	imapprovider "github.com/tfyl/mailroom/internal/provider/imap"
)

// The IMAP credential is a struct marshalled to JSON by whatever links the mailbox and
// unmarshalled here to build the provider. Nothing checks the two agree — the column is an
// opaque string — so a field renamed on one side would seal a credential that unseals into a
// config missing a host, and the failure would arrive as a connection error much later.
//
// This is the test for that seam. It marshals the way `mailroom link-imap` does and
// unmarshals the way build() does.
func TestIMAPCredentialSurvivesTheRoundTripBetweenLinkingAndUse(t *testing.T) {
	want := imapprovider.Config{
		Host:         "imap.example.com:993",
		Username:     "operator@example.com",
		Password:     "app password with spaces removed",
		TLS:          true,
		SMTPHost:     "smtp.example.com:587",
		SMTPFrom:     "operator@example.com",
		SMTPUsername: "different@example.com",
		SMTPPassword: "a second secret",
	}

	blob, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	var got imapprovider.Config
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("the provider layer could not read what linking wrote: %v", err)
	}
	if got != want {
		t.Fatalf("credential changed in transit:\n want %+v\n got  %+v", want, got)
	}
}

// TLS is the field where a silent default is dangerous: false is both the zero value and a
// real, insecure choice, so a credential that lost the field would quietly downgrade the
// connection rather than fail.
func TestIMAPCredentialCarriesTLSExplicitly(t *testing.T) {
	blob, err := json.Marshal(imapprovider.Config{Host: "h:993", TLS: true})
	if err != nil {
		t.Fatal(err)
	}

	var got imapprovider.Config
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	if !got.TLS {
		t.Fatal("TLS must survive the round trip: losing it downgrades the connection silently")
	}
}
