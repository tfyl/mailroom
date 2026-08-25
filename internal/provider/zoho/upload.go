package zoho

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// --- outgoing attachments ---
//
// Zoho does not carry attachment bytes in the message it sends. Files go up first, to a store
// of their own, and the compose call references what came back:
//
//	POST /accounts/{id}/messages/attachments?uploadType=multipart
//	     multipart/form-data, one "attach" part per file
//	  →  data[] of {storeName, attachmentName, attachmentPath, attachmentSize}
//
//	POST /accounts/{id}/messages
//	     "attachments": [{storeName, attachmentPath, attachmentName}, …]
//
// Both halves are Zoho's published contract, field names included:
// https://www.zoho.com/mail/help/api/post-upload-attachments.html for the upload and
// https://www.zoho.com/mail/help/api/post-send-email-attachment.html for the reference, which
// says in as many words that "the response values, storeName, attachmentName and
// attachmentPath from that API should be used in the Request Body of this API".
//
// None of it has been run against a live mailbox. That is the reverse of the rest of this
// package, where a comment saying what Zoho answered means somebody watched it answer, and it
// is the first thing to distrust if a send with attachments misbehaves — this provider has
// already shipped three bugs that came from believing these pages. What is claimed here is
// only that the pages say this.

// zohoMaxOutgoingBytes is the ceiling Zoho publishes for an outgoing message, attachments
// included: 20 MB on a personal account, with the paid plans higher
// (https://www.zoho.com/mail/help/attachments.html).
//
// Held loosely on purpose. The page does not say whether 20 MB means 20×1024² or 20×10⁶, nor
// whether the count is of the raw bytes or of the base64 they become inside a MIME message —
// and if it is the latter, a message well under this can still be refused by Zoho. It is here
// to be compared against mailroom's own limit rather than to be trusted to the byte, and the
// comparison is the part that has to be right.
const zohoMaxOutgoingBytes = 20 << 20

// attachUploads uploads an outgoing message's attachments and records them in the compose
// body, or fails having sent nothing.
//
// The ordering is the whole point. Every file is stored before the compose call is made, so a
// file Zoho would not take stops the message instead of travelling without it — which is the
// failure this path exists to prevent, and the reason Send and CreateDraft refused outright
// until there was an upload to call.
func (p *Provider) attachUploads(ctx context.Context, out mmail.Outgoing, body map[string]any) error {
	if len(out.Attachments) == 0 {
		return nil
	}
	if err := p.attachmentsTooLarge(out.Attachments); err != nil {
		return err
	}

	uploaded, err := p.uploadAttachments(ctx, out.Attachments)
	if err != nil {
		return err
	}

	refs := make([]map[string]any, 0, len(uploaded))
	for _, u := range uploaded {
		refs = append(refs, map[string]any{
			"storeName":      u.StoreName.String(),
			"attachmentPath": u.Path,
			"attachmentName": u.Name,
		})
	}
	body["attachments"] = refs
	return nil
}

// attachmentsTooLarge refuses a message carrying more than will be accepted, before anything
// is uploaded.
//
// Two limits apply and they are not the same number: mailroom caps the attachment content on
// one message at mail.MaxAttachmentBytes, and Zoho caps the assembled message. Refusing at the
// lower of the two is the only rule that stays right when either of them moves, and naming
// which one did the refusing is what stops somebody shrinking a file against the wrong
// ceiling. Today it is mailroom's, and a caller told "Zoho will not take this" would go
// looking at their plan for a limit that is not the one in the way.
func (p *Provider) attachmentsTooLarge(atts []mmail.Attachment) error {
	var total int64
	for _, a := range atts {
		total += int64(len(a.Content))
	}

	limit, whose := int64(mmail.MaxAttachmentBytes), "mailroom"
	if zohoMaxOutgoingBytes < limit {
		limit, whose = zohoMaxOutgoingBytes, "Zoho"
	}
	if total <= limit {
		return nil
	}

	return &mmail.UnsupportedError{
		Provider: mmail.ProviderZoho, Account: p.account.Alias,
		Address: p.account.Address, Capability: mmail.CapSend,
		Op: fmt.Sprintf("sending %d attachments totalling %d bytes", len(atts), total),
		Reason: fmt.Sprintf("%s caps one message's attachments at %d bytes and this is over it; "+
			"send the larger files separately or share them by link", whose, limit),
	}
}

// uploadedAttachment is one entry from Zoho's upload response — the handle a compose call
// references a stored file by.
//
// storeName is a flexString because every other identifier in this package is spelled as a
// JSON string on one endpoint and a bare number on another, and Zoho's own samples show two
// unrelated shapes for this one: "52882865" on the upload page and "NN2:-167775813820412438"
// on the send page. The first of those decodes as a number, and a number is where this
// package has already lost digits off an id. Nothing here depends on which arrives.
type uploadedAttachment struct {
	StoreName flexString `json:"storeName"`
	Name      string     `json:"attachmentName"`
	Path      string     `json:"attachmentPath"`
}

// uploadAttachments posts every file in one multipart request and returns Zoho's handles for
// them, in the order they were sent.
func (p *Provider) uploadAttachments(ctx context.Context, atts []mmail.Attachment) ([]uploadedAttachment, error) {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for _, a := range atts {
		part, err := writer.CreatePart(attachmentPartHeader(a))
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(a.Content); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	query := url.Values{}
	// Zoho documents two upload methods on this one route and keys the response shape off
	// which was used: multipart takes repeated "attach" parts and answers with an array, and
	// the raw method takes one file in the body with its name in the query. Only the first
	// can carry a message's whole attachment set in one request, so it is the only one used —
	// a per-file loop would leave files already stored behind when one of them failed.
	query.Set("uploadType", "multipart")
	// isInline is a property of the request rather than of a file inside it, and everything
	// here goes up as an ordinary attachment. mailroom never rewrites a body to reference an
	// embedded part, so a file stored inline would be one the message never mentions: in the
	// store, and invisible to whoever received the mail. An image that was inline on the
	// message it was forwarded from therefore arrives as an attachment, which is a worse
	// rendering of a file that is still there rather than a file that is gone.
	query.Set("isInline", "false")

	path := "/accounts/" + p.accountID + "/messages/attachments"
	op := "POST " + path

	// Not p.do. That helper JSON-encodes whatever it is given and declares
	// Content-Type: application/json, and this request is multipart/form-data with a boundary
	// only the writer above knows — the same reason GetAttachment writes its own request for
	// the download direction.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+path+"?"+query.Encode(), &form)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, p.wrap(op, 0, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, p.wrap(op, resp.StatusCode, err)
	}
	if resp.StatusCode >= 300 {
		return nil, p.wrap(op, resp.StatusCode, fmt.Errorf("%s: %s", resp.Status, snippet(raw)))
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decoding the zoho attachment upload response: %w", err)
	}
	// The envelope is checked as well as the HTTP status because the two disagree: a Zoho
	// endpoint can answer 200 with a failure inside it, and reading that as a success here
	// would put a message on the wire referencing files that were never stored.
	if env.Status.Code != 0 && env.Status.Code >= 300 {
		return nil, p.wrap(op, env.Status.Code, fmt.Errorf("%s", env.Status.Description))
	}

	// The array is the multipart method's documented shape and the bare object is the raw
	// method's. Both are accepted because this package has already been caught by an account
	// answering /attachmentinfo in the shape the other method's page documents — trying each
	// is less clever than picking one and is the difference between an upload and silence.
	var uploaded []uploadedAttachment
	if err := json.Unmarshal(env.Data, &uploaded); err != nil {
		var single uploadedAttachment
		if err := json.Unmarshal(env.Data, &single); err != nil {
			return nil, fmt.Errorf("decoding the zoho attachment upload response: %w", err)
		}
		uploaded = []uploadedAttachment{single}
	}

	// A short answer means some of the files are not in the store, and going on would compose
	// a message missing exactly the ones Zoho did not mention. Nothing has been sent at this
	// point and this is the last moment that stays true.
	if len(uploaded) != len(atts) {
		return nil, fmt.Errorf("zoho stored %d of this message's %d attachments; nothing has "+
			"been sent", len(uploaded), len(atts))
	}
	for i, u := range uploaded {
		// All three fields are needed to reference a stored file, so an entry missing any of
		// them addresses nothing. Sending it anyway would hand Zoho a reference it cannot
		// resolve, and the recipient would be the one to discover it.
		if u.StoreName == "" || u.Path == "" || u.Name == "" {
			return nil, fmt.Errorf("zoho stored attachment %d (%q) without the store name, path "+
				"and name needed to attach it; nothing has been sent", i+1, atts[i].Filename)
		}
	}
	return uploaded, nil
}

// attachmentPartHeader builds the multipart part for one file.
//
// Not CreateFormFile, which hard-codes application/octet-stream as the part's type. Zoho's
// upload takes no content-type parameter of its own, so the part header is the only place the
// type mailroom is holding can be stated, and a spreadsheet that arrives as an unnamed binary
// blob is a worse attachment than one that arrives as a spreadsheet.
func attachmentPartHeader(a mmail.Attachment) textproto.MIMEHeader {
	filename := a.Filename
	if filename == "" {
		// Zoho stores the file under the name it was uploaded with and echoes that name back
		// as the handle the compose call references, so a part with no filename has nothing
		// to be referenced by afterwards.
		filename = "attachment"
	}
	mimeType := a.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition",
		`form-data; name="attach"; filename="`+formValueEscaper.Replace(filename)+`"`)
	header.Set("Content-Type", mimeType)
	return header
}

// formValueEscaper is mime/multipart's own quoting rule, which is unexported there. A
// filename containing a quote would otherwise close the parameter early, and the file would
// be stored under a truncated name — or the part rejected — for a name that is legal on every
// filesystem mailroom reads attachments from.
var formValueEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")
