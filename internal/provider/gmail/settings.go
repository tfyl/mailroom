package gmail

import (
	"context"
	"strings"

	"google.golang.org/api/gmail/v1"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// --- FilterManager ---

func (p *Provider) ListFilters(ctx context.Context) ([]mmail.Filter, error) {
	resp, err := p.svc.Users.Settings.Filters.List("me").Context(ctx).Do()
	if err != nil {
		return nil, p.wrapSettings("list_filters", err)
	}
	out := make([]mmail.Filter, 0, len(resp.Filter))
	for _, f := range resp.Filter {
		out = append(out, convertFilter(f))
	}
	return out, nil
}

func (p *Provider) CreateFilter(ctx context.Context, f mmail.Filter) (mmail.Filter, error) {
	created, err := p.svc.Users.Settings.Filters.Create("me", &gmail.Filter{
		Criteria: &gmail.FilterCriteria{
			From:          f.From,
			To:            f.To,
			Subject:       f.Subject,
			Query:         f.Query,
			NegatedQuery:  f.NegatedQuery,
			HasAttachment: f.HasAttachment,
		},
		Action: &gmail.FilterAction{
			AddLabelIds:    labelStrings(f.AddLabels),
			RemoveLabelIds: labelStrings(f.RemoveLabels),
			Forward:        f.Forward,
		},
	}).Context(ctx).Do()
	if err != nil {
		return mmail.Filter{}, p.wrapSettings("create_filter", err)
	}
	return convertFilter(created), nil
}

func (p *Provider) DeleteFilter(ctx context.Context, id string) error {
	return p.wrapSettings("delete_filter", p.svc.Users.Settings.Filters.Delete("me", id).Context(ctx).Do())
}

func convertFilter(f *gmail.Filter) mmail.Filter {
	out := mmail.Filter{ID: f.Id}
	if c := f.Criteria; c != nil {
		out.From, out.To, out.Subject = c.From, c.To, c.Subject
		out.Query, out.NegatedQuery, out.HasAttachment = c.Query, c.NegatedQuery, c.HasAttachment
	}
	if a := f.Action; a != nil {
		out.AddLabels = toLabelIDs(a.AddLabelIds)
		out.RemoveLabels = toLabelIDs(a.RemoveLabelIds)
		out.Forward = a.Forward
	}
	return out
}

// --- SettingsManager ---

func (p *Provider) ListSendAs(ctx context.Context) ([]mmail.SendAs, error) {
	resp, err := p.svc.Users.Settings.SendAs.List("me").Context(ctx).Do()
	if err != nil {
		return nil, p.wrapSettings("list_send_as", err)
	}
	out := make([]mmail.SendAs, 0, len(resp.SendAs))
	for _, s := range resp.SendAs {
		out = append(out, mmail.SendAs{
			Address:     s.SendAsEmail,
			DisplayName: s.DisplayName,
			ReplyTo:     s.ReplyToAddress,
			Default:     s.IsDefault,
			Primary:     s.IsPrimary,
			// Gmail leaves this empty for the primary address, which never needs verifying.
			Verified: s.IsPrimary || s.VerificationStatus == "accepted",
		})
	}
	return out, nil
}

func (p *Provider) GetVacation(ctx context.Context) (mmail.Vacation, error) {
	v, err := p.svc.Users.Settings.GetVacation("me").Context(ctx).Do()
	if err != nil {
		return mmail.Vacation{}, p.wrapSettings("get_vacation", err)
	}
	body := v.ResponseBodyPlainText
	if body == "" {
		body = v.ResponseBodyHtml
	}
	return mmail.Vacation{
		Enabled:            v.EnableAutoReply,
		Subject:            v.ResponseSubject,
		Body:               body,
		RestrictToContacts: v.RestrictToContacts,
		RestrictToDomain:   v.RestrictToDomain,
	}, nil
}

// SetVacation replaces the auto-reply.
//
// The restrictions are sent every time rather than only when true: Gmail patches from the
// object it is given, so omitting a false value would silently leave a previous restriction
// in place and quietly widen who gets replies.
func (p *Provider) SetVacation(ctx context.Context, v mmail.Vacation) error {
	_, err := p.svc.Users.Settings.UpdateVacation("me", &gmail.VacationSettings{
		EnableAutoReply:       v.Enabled,
		ResponseSubject:       v.Subject,
		ResponseBodyPlainText: v.Body,
		RestrictToContacts:    v.RestrictToContacts,
		RestrictToDomain:      v.RestrictToDomain,
		ForceSendFields:       []string{"EnableAutoReply", "RestrictToContacts", "RestrictToDomain"},
	}).Context(ctx).Do()
	return p.wrapSettings("set_vacation", err)
}

// --- DelegateManager ---

func (p *Provider) ListDelegates(ctx context.Context) ([]mmail.Delegate, error) {
	resp, err := p.svc.Users.Settings.Delegates.List("me").Context(ctx).Do()
	if err != nil {
		return nil, p.wrapSettings("list_delegates", err)
	}
	out := make([]mmail.Delegate, 0, len(resp.Delegates))
	for _, d := range resp.Delegates {
		out = append(out, mmail.Delegate{
			Address:  d.DelegateEmail,
			Verified: d.VerificationStatus == "accepted",
		})
	}
	return out, nil
}

// --- ForwardingReader ---

func (p *Provider) GetForwarding(ctx context.Context) (mmail.Forwarding, error) {
	f, err := p.svc.Users.Settings.GetAutoForwarding("me").Context(ctx).Do()
	if err != nil {
		return mmail.Forwarding{}, p.wrapSettings("get_forwarding", err)
	}
	return mmail.Forwarding{
		Enabled:     f.Enabled,
		Address:     f.EmailAddress,
		Disposition: f.Disposition,
	}, nil
}

// --- IMAPSettingsReader ---

func (p *Provider) GetIMAPSettings(ctx context.Context) (mmail.IMAPSettings, error) {
	s, err := p.svc.Users.Settings.GetImap("me").Context(ctx).Do()
	if err != nil {
		return mmail.IMAPSettings{}, p.wrapSettings("get_imap", err)
	}
	return mmail.IMAPSettings{
		Enabled:       s.Enabled,
		AutoExpunge:   s.AutoExpunge,
		MaxFolderSize: s.MaxFolderSize,
	}, nil
}

// wrapSettings adds one distinction to the ordinary error mapping: a refusal that no retry
// can fix, reported as unsupported rather than as a failure.
//
// Two separate things land here, both permanent for a given mailbox. The first is a missing
// OAuth scope — delegation needs gmail.settings.sharing, which mailroom does not request, and
// no amount of re-linking adds a scope the deployment never asks for. The second only shows
// up against a real account: on a consumer gmail.com mailbox, delegation is refused outright
// with "access restricted to service accounts that have been delegated domain-wide
// authority", because it is a Google Workspace feature that personal accounts do not have at
// all. Neither is a transient error, and reporting them as one sends a caller round a loop
// against something that will never work.
func (p *Provider) wrapSettings(op string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	for _, permanent := range []struct{ match, reason string }{
		{"access restricted to service accounts",
			"Gmail allows this only for a Workspace account reached through domain-wide delegation"},
		{"domain-wide authority",
			"Gmail allows this only for a Workspace account reached through domain-wide delegation"},
		{"insufficient authentication scopes",
			"the mailbox was linked without the scope this needs; re-link it to grant one"},
		{"insufficientpermissions",
			"the mailbox was linked without the scope this needs; re-link it to grant one"},
		{"request had insufficient authentication",
			"the mailbox was linked without the scope this needs; re-link it to grant one"},
	} {
		if strings.Contains(msg, permanent.match) {
			// Named by operation rather than by capability. Gmail implements all of these
			// and refuses individual ones per account, so a caller told that settings are
			// unsupported would stop trying the five that work.
			return &mmail.UnsupportedError{
				Provider:   mmail.ProviderGmail,
				Account:    p.account.Alias,
				Address:    p.account.Address,
				Capability: mmail.CapSettings,
				Op:         op,
				Reason:     permanent.reason,
			}
		}
	}
	return p.wrap(op, err)
}

func toLabelIDs(in []string) []mmail.LabelID {
	if len(in) == 0 {
		return nil
	}
	out := make([]mmail.LabelID, len(in))
	for i, s := range in {
		out[i] = mmail.LabelID(s)
	}
	return out
}

var _ interface {
	mmail.FilterManager
	mmail.SettingsManager
	mmail.DelegateManager
	mmail.ForwardingReader
	mmail.IMAPSettingsReader
} = (*Provider)(nil)
