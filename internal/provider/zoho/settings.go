package zoho

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// --- SettingsManager ---
//
// Zoho has no settings API of its own. Both things this interface asks for live on the
// account record: the from-addresses are `sendMailDetails` and the auto-reply is
// `vacationResponse`, and both arrive from GET /api/accounts
// (https://www.zoho.com/mail/help/api/get-all-users-accounts.html). Writing the auto-reply is
// a PUT back to /api/accounts/{accountId} carrying a `mode`, which is the shape Zoho's whole
// account surface uses — the same index lists sixteen other modes for forwarding, send-mail
// details and the reply-to address (https://www.zoho.com/mail/help/api/account-api.html).
//
// Nothing in this file has been run against a live Zoho mailbox. The field names and the two
// request bodies come from the pages cited at each method; where Zoho's published samples
// disagree with its own prose, or say nothing at all, that is called out where it is relied
// on rather than smoothed over. The rest of this package is a standing argument for reading
// those markers seriously: three of its bugs were written against documentation and passed a
// stub that had been told what to answer.

// accountRecord is the part of Zoho's account object mailroom reads. Only the fields used
// are decoded; the published record carries about fifty more.
type accountRecord struct {
	// Zoho spells its ids as strings on some endpoints and bare numbers on others, and
	// nothing says which this one is. See flexString.
	AccountID   flexString         `json:"accountId"`
	PrimaryMail string             `json:"primaryEmailAddress"`
	SendMail    []sendMailDetail   `json:"sendMailDetails"`
	Vacation    []vacationResponse `json:"vacationResponse"`
}

type sendMailDetail struct {
	FromAddress string `json:"fromAddress"`
	DisplayName string `json:"displayName"`
	Validated   bool   `json:"validated"`
}

// vacationResponse is the stored auto-reply. It carries no enabled flag of any spelling —
// see GetVacation for what that costs and why the alternative is worse.
type vacationResponse struct {
	Subject string `json:"subject"`
	Content string `json:"content"`
}

// accountRecord reads the record Zoho keeps for this mailbox.
//
// The listing endpoint rather than GET /api/accounts/{accountId}, though both are documented
// to answer with `sendMailDetails`, because only the listing's published sample carries
// `vacationResponse` at all: the single-account page shows the same mailbox — same address,
// same account id — with no auto-reply field anywhere in the response
// (https://www.zoho.com/mail/help/api/get-user-account-details.html against
// https://www.zoho.com/mail/help/api/get-all-users-accounts.html). Which of the two the
// service really answers with is unverified. Reading the endpoint that is documented to carry
// the field is the choice that cannot report an auto-reply as absent for a mailbox that has
// one, which is the failure worth avoiding: a caller told the responder is off has no reason
// to look again.
//
// The record is picked by account id rather than taken first. A single Zoho login holds
// several accounts — the published sample answers with a Zoho mailbox and an IMAP one, each
// carrying its own `vacationResponse` — so taking the first would report another mailbox's
// aliases and another mailbox's auto-reply under this one's name.
func (p *Provider) accountRecord(ctx context.Context) (accountRecord, error) {
	var accounts []accountRecord
	if err := p.get(ctx, "/accounts", nil, &accounts); err != nil {
		return accountRecord{}, err
	}
	for _, a := range accounts {
		if a.AccountID.String() == p.accountID {
			return a, nil
		}
	}
	return accountRecord{}, fmt.Errorf("zoho answered with %d account(s) and none of them is %s, "+
		"the mailbox this provider was built for", len(accounts), p.accountID)
}

// ListSendAs reports the addresses this mailbox can send from.
//
// `sendMailDetails` is a genuine send-as list, which is why this reads it rather than the
// `emailAddress` array beside it. That array is the receiving side — Zoho marks each entry
// `isAlias`, `isPrimary` and `isConfirmed` — and an address a mailbox collects mail at is not
// an address it may put in a From header. Microsoft has only the receiving list and says so;
// Zoho has both, so this answers the question that was actually asked.
//
// ReplyTo is left empty throughout. Zoho has a reply-to address — there is a whole
// `updateReplyToStatus` mode for writing one
// (https://www.zoho.com/mail/help/api/put-update-reply-to-address.html) — but no published
// account response carries a field for reading it back, and guessing at a name would either
// decode nothing or decode the wrong thing.
func (p *Provider) ListSendAs(ctx context.Context) ([]mmail.SendAs, error) {
	record, err := p.accountRecord(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]mmail.SendAs, 0, len(record.SendMail))
	for _, s := range record.SendMail {
		if s.FromAddress == "" {
			// An entry with no address names nothing a caller could send from.
			continue
		}
		// Identified against `primaryEmailAddress` from the same record rather than against
		// the address mailroom stored, so the answer is Zoho's rather than mailroom's, and
		// rather than against the row's `mode` field: the read sample spells the mailbox's own
		// row `mailbox` while the write API documents `extmailbox` and `extfrom` and never
		// mentions `mailbox`, so the two vocabularies are not the same one.
		primary := record.PrimaryMail != "" && strings.EqualFold(s.FromAddress, record.PrimaryMail)

		out = append(out, mmail.SendAs{
			Address:     s.FromAddress,
			DisplayName: s.DisplayName,
			// Zoho publishes no field saying which from-address is the default for composing.
			// `status` is the nearest-looking candidate and it is not that flag: Zoho's own
			// sample has it true on two rows at once. So the mailbox's own address is reported
			// as the default and nothing else is, which is mailroom's reading rather than
			// something Zoho said.
			Default: primary,
			Primary: primary,
			// `validated` is the only field whose name is about verification, and Zoho
			// documents nothing about it beyond its appearance in a response sample — where it
			// is false, with `validationRequired` true, on the mailbox's own address. So it
			// cannot be read alone: doing that reports the address the account *is* as one
			// nothing has established it can send from, and a caller that filters on Verified
			// would be left unable to send at all.
			//
			// Every other address is verified only where Zoho says `validated`. An alias
			// reported as verified when it is not is a send that fails after it was accepted,
			// so an entry Zoho is quiet about stays unverified.
			Verified: primary || s.Validated,
		})
	}
	return out, nil
}

// GetVacation reads the stored auto-reply.
//
// Two of mailroom's five fields cannot be answered from what Zoho returns, and both are
// reported as they are rather than guessed at:
//
// Enabled is presence. Zoho's stored response carries no enabled flag — the sample's fields
// are `subject`, `content`, `sendTo`, `fromDate`, `toDate`, `infiniteDate`, `replyType`,
// `interval`, `intervalType`, `markBusy`, `includeSignature`, `name` and `vacationId`, and
// none of them is a switch. What decides whether a reply goes out is the date window, and the
// dates cannot be read: Zoho's own write API documents the format as MM/DD/YYYY HH:MM:SS and
// then gives `"toDate": "19/05/2024"` as its sample
// (https://www.zoho.com/mail/help/api/put-add-vacation-reply.html), which has no nineteenth
// month in it. With the field order contradicted by the page that defines it, parsing one to
// decide "is this in effect today" would be a coin toss dressed as a fact. So a stored
// response reads as enabled, and mailroom over-reports an expired one. That is the direction
// that leaves somebody checking, rather than the direction that tells them an auto-reply they
// still have is switched off.
//
// The restrictions are reported as unset. Zoho writes the audience as a named string —
// `all`, `contacts`, `noncontacts`, `org`, `nonOrgAll`, `nonOrgContacts`, `nonOrgNonContacts`
// — and reads it back as a bare integer, and no page documents which number is which name.
// Reporting no restriction says the reply goes to everyone, which is the wider of the two
// claims: a caller reading it is told the auto-reply reaches more people than it might,
// rather than fewer.
func (p *Provider) GetVacation(ctx context.Context) (mmail.Vacation, error) {
	record, err := p.accountRecord(ctx)
	if err != nil {
		return mmail.Vacation{}, err
	}
	if len(record.Vacation) == 0 {
		return mmail.Vacation{}, nil
	}
	// Zoho returns an array and its samples hold one entry; nothing documents what a second
	// would mean. The first is the only one that can be reported through a model with room
	// for one auto-reply, and inventing a rule for choosing between several would be a guess
	// at a situation nobody has seen.
	stored := record.Vacation[0]
	return mmail.Vacation{Enabled: true, Subject: stored.Subject, Body: stored.Content}, nil
}

// SetVacation switches the auto-reply off, and refuses to switch one on.
//
// Off is exact. Zoho's `deleteVacationReply` takes nothing but the mode under user
// authentication (https://www.zoho.com/mail/help/api/put-delete-vacation-reply.html), so
// there is no part of the request mailroom has to invent. It removes the stored response
// rather than disabling it, so the wording does not survive being switched off — Zoho has no
// state for an auto-reply that exists and is quiet, and turning one back on means supplying
// the text again.
//
// On is refused, because Zoho requires three things mailroom's model does not carry and one
// of them cannot be written confidently even with a value to hand. `fromDate`, `toDate` and
// `sendingInt` are all mandatory on add and update
// (https://www.zoho.com/mail/help/api/put-add-vacation-reply.html,
// https://www.zoho.com/mail/help/api/put-update-vacation-reply.html), and mailroom's Vacation
// has no dates at all. Choosing them means deciding on somebody's behalf when their
// auto-reply stops answering their mail — either it goes quiet while they are still away, or
// it answers strangers for a year after they are back — and the caller is never told which
// was picked. The date format makes it worse rather than better: the same page that calls it
// MM/DD/YYYY HH:MM:SS gives `19/05/2024` as its own sample, so a date mailroom wrote could be
// read as a different day from the one it meant.
//
// This is the refusal CreateDraft makes about replies and the one Send makes about
// attachments: where the request cannot be carried out as asked, saying so beats doing
// something adjacent and reporting success.
func (p *Provider) SetVacation(ctx context.Context, v mmail.Vacation) error {
	if v.Enabled {
		return &mmail.UnsupportedError{
			Provider: mmail.ProviderZoho, Account: p.account.Alias,
			Address: p.account.Address, Capability: mmail.CapSettings,
			Op: "switching a vacation reply on",
			Reason: "Zoho requires a start date, an end date and a sending interval on every " +
				"vacation reply, and mailroom's vacation settings carry none of them; picking " +
				"dates here would decide when the auto-reply stops answering without telling " +
				"you which dates were chosen. Set it in Zoho's own client, where the dates are " +
				"yours to choose; mailroom can still read it back and switch it off",
		}
	}

	// Asked first, because deleting a vacation reply that is not there is not a no-op at
	// Zoho's end — it is a 500:
	//
	//	PUT /accounts/{id}: 500 Internal Server Error
	//	{"status":{"code":500,"description":"Internal Error"},"data":{"moreInfo":"Internal Error"}}
	//
	// Measured against the live mailbox, which had no responder set. That is the common case
	// by a distance: most mailboxes have no auto-reply most of the time, so without this the
	// only write this provider supports failed almost every time it was called, with an error
	// that reads like Zoho being broken rather than like nothing needing to be done.
	//
	// Switching off something already off is a request that has been satisfied, so it is
	// reported as one. A read that fails is not treated as "already off" — that would report
	// success for a responder still running, which is the direction that matters.
	current, err := p.GetVacation(ctx)
	if err != nil {
		return err
	}
	if !current.Enabled {
		return nil
	}

	// Decoded into something rather than discarded, because do only inspects Zoho's envelope
	// when it has somewhere to put the data — and a Zoho endpoint can answer HTTP 200 with a
	// failing envelope inside it. A switch-off that failed in the envelope and was read as a
	// success would leave the auto-reply running and report it stopped. Zoho's documented
	// response for this mode is the bare envelope with no data at all, which do handles; this
	// is here for the envelope check rather than for a value.
	//
	// This half is still unverified: switching off a responder that is genuinely on has not
	// been run, because turning one on to test it would need dates mailroom refuses to invent
	// and would leave somebody's mailbox auto-replying if the switch-off then failed.
	var acknowledged json.RawMessage
	body := map[string]any{"mode": "deleteVacationReply"}
	return p.do(ctx, http.MethodPut, "/accounts/"+p.accountID, nil, body, &acknowledged)
}

var _ mmail.SettingsManager = (*Provider)(nil)
