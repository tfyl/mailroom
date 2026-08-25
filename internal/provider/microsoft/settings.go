package microsoft

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// --- FilterManager ---
//
// Exchange calls these message rules, and keeps them on the inbox: /me/mailFolders/inbox/
// messageRules is where Outlook's own "Rules" live. They are the reason this provider talks
// to Graph rather than to IMAP, where mail_filters has nothing at all to address.

type messageRule struct {
	ID          string          `json:"id,omitempty"`
	DisplayName string          `json:"displayName,omitempty"`
	Sequence    int             `json:"sequence,omitempty"`
	IsEnabled   bool            `json:"isEnabled"`
	Conditions  *rulePredicates `json:"conditions,omitempty"`
	Exceptions  *rulePredicates `json:"exceptions,omitempty"`
	Actions     *ruleActions    `json:"actions,omitempty"`
}

type rulePredicates struct {
	SenderContains    []string `json:"senderContains,omitempty"`
	RecipientContains []string `json:"recipientContains,omitempty"`
	SubjectContains   []string `json:"subjectContains,omitempty"`
	BodyContains      []string `json:"bodyContains,omitempty"`
	HasAttachments    bool     `json:"hasAttachments,omitempty"`
}

type ruleActions struct {
	MoveToFolder     string      `json:"moveToFolder,omitempty"`
	AssignCategories []string    `json:"assignCategories,omitempty"`
	ForwardTo        []recipient `json:"forwardTo,omitempty"`
	Delete           bool        `json:"delete,omitempty"`
	MarkAsRead       bool        `json:"markAsRead,omitempty"`
	StopProcessing   bool        `json:"stopProcessingRules,omitempty"`
}

// deletedItems is Graph's well-known name for the Deleted Items folder, which move and copy
// accept in place of an id.
const deletedItems = "deleteditems"

func (p *Provider) ListFilters(ctx context.Context) ([]mmail.Filter, error) {
	var page struct {
		Value []messageRule `json:"value"`
	}
	if err := p.getSettings(ctx, "/me/mailFolders/inbox/messageRules", nil, &page, "list_filters"); err != nil {
		return nil, err
	}
	out := make([]mmail.Filter, 0, len(page.Value))
	for _, r := range page.Value {
		out = append(out, convertRule(r))
	}
	return out, nil
}

// CreateFilter writes a rule.
//
// Two things the canonical Filter can express and a message rule cannot, both refused by name
// rather than dropped. A rule adds and never removes, so a filter that asks to take a label
// off cannot be honoured; and a rule's exceptions are their own predicates rather than a
// negated query, so a NegatedQuery is mapped onto an exception on the body and nothing else.
func (p *Provider) CreateFilter(ctx context.Context, f mmail.Filter) (mmail.Filter, error) {
	if len(f.RemoveLabels) > 0 {
		return mmail.Filter{}, p.unsupported(mmail.CapFilters,
			"a filter that removes a label",
			"an Outlook message rule can move, categorise, forward and delete, but it has no "+
				"action that takes a category or a folder away")
	}

	actions := &ruleActions{}
	for _, id := range f.AddLabels {
		kind, native, err := splitLabelID(id)
		if err != nil {
			return mmail.Filter{}, err
		}
		if kind == labelCategory {
			actions.AssignCategories = append(actions.AssignCategories, native)
			continue
		}
		if native == deletedItems {
			// A rule has its own delete action, and it is not the same thing as a move: Graph
			// documents moveToFolder as taking a folder id, where move and copy on a message
			// also accept a well-known name. Writing "deleteditems" into moveToFolder would be
			// relying on something nobody has written down, and it would come back out of
			// ListFilters as a different rule from the one that went in.
			actions.Delete = true
			continue
		}
		if actions.MoveToFolder != "" && actions.MoveToFolder != native {
			return mmail.Filter{}, fmt.Errorf("a rule can move mail to one folder; asked for both %q and %q",
				actions.MoveToFolder, native)
		}
		actions.MoveToFolder = native
	}
	if f.Forward != "" {
		var to recipient
		to.EmailAddress.Address = f.Forward
		actions.ForwardTo = []recipient{to}
	}
	if actions.MoveToFolder == "" && !actions.Delete &&
		len(actions.AssignCategories) == 0 && len(actions.ForwardTo) == 0 {
		// Exchange refuses a rule with no actions, and a filter that matches mail and then
		// does nothing to it is not something a caller meant to create.
		return mmail.Filter{}, fmt.Errorf("a filter needs at least one action: a folder to move to, a category to assign, or an address to forward to")
	}

	conditions := &rulePredicates{HasAttachments: f.HasAttachment}
	appendIf(&conditions.SenderContains, f.From)
	appendIf(&conditions.RecipientContains, f.To)
	appendIf(&conditions.SubjectContains, f.Subject)
	appendIf(&conditions.BodyContains, f.Query)

	rule := messageRule{
		// A rule must be named, and the canonical Filter carries no name — so one is written
		// from what the rule does. A row in Outlook's own rules list reading "mailroom: from
		// alerts@example.com" is a good deal easier to place than an unnamed one.
		DisplayName: ruleName(f),
		IsEnabled:   true,
		Sequence:    p.nextSequence(ctx),
		Conditions:  conditions,
		Actions:     actions,
	}
	if f.NegatedQuery != "" {
		rule.Exceptions = &rulePredicates{BodyContains: []string{f.NegatedQuery}}
	}

	var created messageRule
	if err := p.doSettings(ctx, http.MethodPost, "/me/mailFolders/inbox/messageRules",
		rule, &created, "create_filter"); err != nil {
		return mmail.Filter{}, err
	}
	return convertRule(created), nil
}

func (p *Provider) DeleteFilter(ctx context.Context, id string) error {
	return p.doSettings(ctx, http.MethodDelete,
		"/me/mailFolders/inbox/messageRules/"+escapeID(id), nil, nil, "delete_filter")
}

// nextSequence picks a position for a new rule. Exchange evaluates rules in sequence order and
// rejects a duplicate, so a new rule goes after every existing one — which is also the least
// surprising place for it, since it cannot then pre-empt a rule somebody already relies on.
//
// A failure to read the existing rules is not fatal here: the create that follows will report
// whatever is really wrong, and a sequence of 1 is a reasonable guess for the mailbox with no
// rules at all that is by far the likeliest reason for an empty answer.
func (p *Provider) nextSequence(ctx context.Context) int {
	var page struct {
		Value []messageRule `json:"value"`
	}
	if err := p.getSettings(ctx, "/me/mailFolders/inbox/messageRules", nil, &page, "list_filters"); err != nil {
		return 1
	}
	next := 1
	for _, r := range page.Value {
		if r.Sequence >= next {
			next = r.Sequence + 1
		}
	}
	return next
}

func ruleName(f mmail.Filter) string {
	var parts []string
	for _, c := range []struct{ label, value string }{
		{"from", f.From}, {"to", f.To}, {"subject", f.Subject}, {"matching", f.Query},
	} {
		if c.value != "" {
			parts = append(parts, c.label+" "+c.value)
		}
	}
	if f.HasAttachment {
		parts = append(parts, "with an attachment")
	}
	if len(parts) == 0 {
		return "mailroom rule"
	}
	name := "mailroom: " + strings.Join(parts, ", ")
	// Exchange caps a rule name at 256 characters and refuses a longer one outright.
	if len(name) > 250 {
		name = name[:250]
	}
	return name
}

func convertRule(r messageRule) mmail.Filter {
	out := mmail.Filter{ID: r.ID}
	if c := r.Conditions; c != nil {
		out.From = strings.Join(c.SenderContains, " ")
		out.To = strings.Join(c.RecipientContains, " ")
		out.Subject = strings.Join(c.SubjectContains, " ")
		out.Query = strings.Join(c.BodyContains, " ")
		out.HasAttachment = c.HasAttachments
	}
	if e := r.Exceptions; e != nil {
		out.NegatedQuery = strings.Join(e.BodyContains, " ")
	}
	if a := r.Actions; a != nil {
		if a.MoveToFolder != "" {
			out.AddLabels = append(out.AddLabels, folderLabel(a.MoveToFolder))
		}
		for _, c := range a.AssignCategories {
			out.AddLabels = append(out.AddLabels, categoryLabel(c))
		}
		if a.Delete {
			// A rule that deletes moves the mail to Deleted Items, which in the label model is
			// exactly an exclusive label being applied. Reporting it that way keeps the shape
			// of a rule the same whichever action it uses to get mail out of the inbox, and it
			// round-trips: CreateFilter turns the same label back into the delete action.
			out.AddLabels = append(out.AddLabels, folderLabel(deletedItems))
		}
		if len(a.ForwardTo) > 0 {
			out.Forward = a.ForwardTo[0].EmailAddress.Address
		}
	}
	return out
}

func appendIf(dst *[]string, v string) {
	if v != "" {
		*dst = append(*dst, v)
	}
}

// --- SettingsManager ---

// ListSendAs reports the addresses this mailbox receives at.
//
// Graph has no send-as API, so this is not the Gmail list of addresses configured for
// sending: it is the mailbox's own address, plus the proxy addresses Exchange routes to it
// where there are any. That is the honest answer to "which addresses is this mailbox", and it
// is not an answer to "which of them will Exchange let you send from" — an alias here is
// marked unverified for exactly that reason, because nothing has established that it can.
func (p *Provider) ListSendAs(ctx context.Context) ([]mmail.SendAs, error) {
	me, err := p.me(ctx)
	if err != nil {
		return nil, err
	}

	primary := me.address()
	out := []mmail.SendAs{{
		Address: primary, DisplayName: me.DisplayName,
		Default: true, Primary: true, Verified: true,
	}}
	for _, proxy := range me.ProxyAddresses {
		// Exchange writes the primary address as SMTP: in capitals and the secondaries as
		// smtp:, and mixes in non-mail schemes such as x500: that are not addresses at all.
		scheme, address, ok := strings.Cut(proxy, ":")
		if !ok || !strings.EqualFold(scheme, "smtp") || strings.EqualFold(address, primary) {
			continue
		}
		out = append(out, mmail.SendAs{Address: address, Verified: false})
	}
	return out, nil
}

type automaticReplies struct {
	Status               string `json:"status"`
	ExternalAudience     string `json:"externalAudience"`
	InternalReplyMessage string `json:"internalReplyMessage"`
	ExternalReplyMessage string `json:"externalReplyMessage"`
}

type mailboxSettings struct {
	AutomaticRepliesSetting *automaticReplies `json:"automaticRepliesSetting,omitempty"`
}

const (
	repliesDisabled = "disabled"
	repliesAlwaysOn = "alwaysEnabled"

	audienceAll         = "all"
	audienceContacts    = "contactsOnly"
	audienceNoneOutside = "none"
)

func (p *Provider) GetVacation(ctx context.Context) (mmail.Vacation, error) {
	query := url.Values{}
	query.Set("$select", "automaticRepliesSetting")

	var settings mailboxSettings
	if err := p.getSettings(ctx, "/me/mailboxSettings", query, &settings, "get_vacation"); err != nil {
		return mmail.Vacation{}, err
	}
	replies := settings.AutomaticRepliesSetting
	if replies == nil {
		return mmail.Vacation{}, nil
	}

	body := replies.ExternalReplyMessage
	if body == "" {
		body = replies.InternalReplyMessage
	}
	return mmail.Vacation{
		Enabled: replies.Status != "" && replies.Status != repliesDisabled,
		// Subject stays empty because there is nothing to put in it: an Outlook automatic
		// reply has no subject line of its own. See SetVacation.
		Body:               body,
		RestrictToContacts: replies.ExternalAudience == audienceContacts,
		RestrictToDomain:   replies.ExternalAudience == audienceNoneOutside,
	}, nil
}

// SetVacation replaces the automatic reply.
//
// A subject is refused rather than dropped. Outlook's automatic replies have no subject of
// their own — the reply carries "Re:" and the original subject, and there is no field for
// anything else — so accepting one would mean quietly not doing what was asked, on a setting
// nobody looks at again until somebody outside mentions the reply they got.
//
// The audience is written on every call, including when neither restriction is set. It is a
// single value rather than two flags, so leaving it alone would let a previous "contacts
// only" survive a request that plainly asked for everyone.
func (p *Provider) SetVacation(ctx context.Context, v mmail.Vacation) error {
	if strings.TrimSpace(v.Subject) != "" {
		return p.unsupported(mmail.CapSettings,
			"a vacation reply with a subject",
			"an Outlook automatic reply has no subject line of its own; it answers with the "+
				"original subject, so put the wording in the body")
	}

	audience := audienceAll
	switch {
	case v.RestrictToDomain:
		audience = audienceNoneOutside
	case v.RestrictToContacts:
		audience = audienceContacts
	}
	status := repliesDisabled
	if v.Enabled {
		status = repliesAlwaysOn
	}

	body := mailboxSettings{AutomaticRepliesSetting: &automaticReplies{
		Status:           status,
		ExternalAudience: audience,
		// Both messages are written. Exchange keeps them apart so a colleague and a stranger
		// can be told different things; mailroom's model has one body, and writing it to only
		// one of the two would leave half the senders getting whatever was there before.
		InternalReplyMessage: v.Body,
		ExternalReplyMessage: v.Body,
	}}
	return p.doSettings(ctx, http.MethodPatch, "/me/mailboxSettings", body, nil, "set_vacation")
}

// getSettings and doSettings are the ordinary transport with one extra distinction applied to
// what comes back. See wrapSettings.

func (p *Provider) getSettings(ctx context.Context, path string, query url.Values, out any, op string) error {
	return p.wrapSettings(op, p.get(ctx, path, query, out))
}

func (p *Provider) doSettings(ctx context.Context, method, path string, body, out any, op string) error {
	return p.wrapSettings(op, p.do(ctx, method, path, nil, body, out))
}

// wrapSettings turns a refusal no retry can fix into an unsupported operation.
//
// Exchange answers a consumer mailbox and a work or school one from the same endpoints and
// then refuses parts of the surface on the first. That refusal arrives as a 403 — which is
// otherwise a permission problem an operator would go looking for a scope to fix, and there is
// no scope to find: the feature is absent from the account.
//
// The 403 has a second documented cause worth naming in the same breath, because it looks
// identical from here and has a completely different fix: an Exchange application access
// policy that blocks this app from the mailbox outright. Re-consenting does nothing about that
// one, so the message says both.
//
// Named by operation rather than by capability, because the rest of the capability works. A
// caller told that settings are unsupported on this mailbox would stop asking for the aliases,
// which are fine.
func (p *Provider) wrapSettings(op string, err error) error {
	if err == nil {
		return nil
	}

	var failed *failure
	if !errors.As(err, &failed) || failed.status != http.StatusForbidden {
		return err
	}

	capability := mmail.CapSettings
	if strings.HasSuffix(op, "_filter") || strings.HasSuffix(op, "_filters") {
		capability = mmail.CapFilters
	}
	return p.unsupported(capability, op,
		"Exchange refused this outright. On a personal Microsoft account message rules and the "+
			"automatic-replies setting are not features that exist, whatever is consented to; "+
			"on a work or school one the usual cause is an Exchange application access policy "+
			"that excludes this app from the mailbox. Neither is fixed by re-linking")
}

var _ interface {
	mmail.FilterManager
	mmail.SettingsManager
} = (*Provider)(nil)
