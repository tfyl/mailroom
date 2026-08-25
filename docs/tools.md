# Tool contract

The MCP surface. Written before implementation on purpose: aggregation and error semantics
are the parts that are painful to change once clients depend on them.

## Discovery

A model cannot use a mailbox it does not know exists, and it should never have to guess what
it is allowed to do. `mail_accounts` answers both, and every session is expected to start
with it.

```jsonc
// mail_accounts  →
{
  "accounts": [
    {
      "alias":    "work",
      "address":  "you@example.com",
      "provider": "gmail",
      "status":   "linked",              // linked | needs_reauth | disabled
      "granted":  ["read", "draft", "labels", "attachments"],
      "provider_supports": [
        "read", "draft", "discard", "send", "labels", "attachments",
        "filters", "settings", "destructive"
      ],
      "quirks": []
    },
    {
      "alias":    "archive",
      "address":  "old@example.net",
      "provider": "imap",
      "status":   "linked",
      "granted":  ["read"],
      "provider_supports": ["read", "draft", "discard", "send", "attachments"],
      "quirks": ["derived_threads", "exclusive_labels", "no_batch"]
    }
  ],
  "default_scope": ["work", "archive"],    // what an omitted `account` reaches
  "mode": {
    "name":     "hold",                    // unattended | confirm | hold
    "means":    "Sending, deleting, filters and the vacation responder are not carried out …",
    "enforced": true,                      // false for the other two — see below
    "held_tools": [
      "mail_send", "mail_trash (trash and delete)",
      "mail_filters (create and delete)", "mail_settings (set_vacation)"
    ],
    "approved_at": "https://mail.example.com/held"
  }
}
```

`default_scope` is bare aliases on purpose. It is not a description of the mailboxes — the
`accounts` block above is that, and carries each address beside its alias — it is the
selector an omitted `account` is equivalent to, and every entry has to be something a caller
can put straight back into the next call.

Three separate facts, deliberately not collapsed:

- **`granted`** — what this grant permits. Narrower than the next field.
- **`provider_supports`** — what the mailbox is capable of at all.
- **`quirks`** — behavioural warnings that change how a model should interpret results.

Keeping `granted` and `provider_supports` apart lets a model tell "you have not given me
permission to send" from "this mailbox cannot do filters at all", and say so accurately
instead of retrying.

`mail_accounts` requires no capability. Knowing which mailboxes a grant reaches is not
privileged; it is a prerequisite for using the grant sensibly.

### The mode

`mode` is the grant's posture: how much this client may do on its own. The three are described
in [grants.md](grants.md#modes); what matters to a caller is `enforced`.

The mode is carried in two places, and deliberately. Each tool's `Description` is worded for
the mode, which is where an agent actually reads it; and it is repeated here because a client
that cached its tool list at connection time and a client that re-reads it on every call are
both real, and this is the call every client is told to make first.

`enforced: false` means the mode is instruction and nothing more — `mail_send` sends. `hold`
is the only one this server refuses on, and under it the tools named in `held_tools` **do not
do what they say**: the call is recorded for the mailbox's owner to approve at `approved_at`,
and the result carries `held: true` with the id it was queued under instead of the usual
`sent`. Report such a call as waiting for approval; a client that reports it as done has told
its user something false about their mail.

The result also carries `expires_at`, and it means what it says: a queued action nobody
answers by then is discarded along with the message it was holding, and cannot be approved
afterwards. It is not a retry deadline — calling again queues a second copy — it is the point
past which "still waiting for approval" stops being a true thing to tell somebody. The field
is absent on an instance that has turned retention off. See
[grants.md](grants.md#how-long-a-held-action-waits).

### Quirks

| Quirk | Meaning for the model |
|---|---|
| `derived_threads` | Threading was inferred, not reported by the provider; a short thread is not proof there were no replies |
| `exclusive_labels` | Applying a label *moves* the message |
| `no_batch` | Batch calls are looped server-side; large batches are slow |
| `partial_search` | Provider search syntax is limited; some queries are approximated |

## The `account` parameter

`mail_search` takes it, and there it accepts four forms:

| Form | Meaning |
|---|---|
| omitted | Every account in the grant. The default. |
| `"work"` | One account, by alias |
| `"you@example.com"` | One account, by address |
| `["work", "archive"]` | An explicit subset |

The rest of the surface is narrower than that, which is worth knowing before writing a call
against it:

- `mail_draft`, `mail_send`, `mail_labels`, `mail_filters` and `mail_settings` take `account`
  as a **plain string** — one alias or one address. A list is refused by the generated schema
  rather than reaching the handler. Omitting it means every account in the grant for
  `mail_labels list`, and means "the only one you have" for the administrative tools, which
  refuse to guess when a grant covers several.
- `mail_get_message`, `mail_get_thread`, `mail_get_attachment`, `mail_modify` and `mail_trash`
  take **no `account` at all**. Every id already names its account, so taking the mailbox from
  the id is one fewer thing a caller can get wrong and one fewer pair of values that can
  disagree. `mail_draft` and `mail_send` do the same for a reply, which takes its mailbox from
  `in_reply_to`.
- `mail_accounts` takes nothing. It reports the whole grant.

Naming an account outside the grant is `scope_denied` — never silently dropped. A model that
asked for two mailboxes and got results from one, with no error, will report to the user as
though it searched both.

There is no `"all"` literal. Omission already means "everything I am allowed to see", and a
magic string that widens as new mailboxes are linked is a footgun.

## Aggregated results

`mail_search` returns this envelope, and it is currently the only tool that does.

```jsonc
{
  "results": [ /* merged, newest first */ ],
  "accounts": {
    "work":    { "status": "ok", "address": "you@example.com", "returned": 18 },
    "archive": { "status": "rate_limited", "address": "old@example.net", "returned": 0,
                 "message": "provider throttled; retry after 30s" }
  },
  "cursor": "eyJ3b3JrIjoi…",              // omitted when nothing remains
  "complete": false
}
```

**Partial failure never fails the call.** One throttled mailbox must not lose the results
from three healthy ones. But the failure must be *visible*: the `accounts` block reports every
account the call touched, and a model that cannot distinguish "no matching mail" from "that
mailbox was unreachable" will confidently tell the user the wrong thing.

If *every* account fails, the call returns an error rather than an empty success.

Per-account statuses: `ok`, `rate_limited`, `auth_expired`, `unsupported`, `timeout`, `error`.

The other tools that reach several mailboxes in one call — `mail_modify`, `mail_trash` and
`mail_labels` — return the `accounts` object alone, keyed by alias, carrying what happened to
each mailbox. No merged `results`, no cursor, and no `status` field, because there is nothing
to merge or paginate.

```jsonc
{
  "accounts": {
    "work":    { "modified": 4, "address": "you@example.com" },
    "archive": { "address": "old@example.net", "error": "auth_expired",
                 "message": "credentials expired; re-link this mailbox" }
  },
  "rejected": [                            // omitted when every id routed
    { "id": "acct_9:1234567890abc", "error": "not_found", "message": "not found" }
  ]
}
```

Three rules there are the same as `mail_search`'s, for the same reasons:

- **Partial failure never fails the call.** One mailbox that refuses does not discard what
  the others already did — and neither does one bad id. `mail_modify` and `mail_trash` take
  ids that name their own mailboxes, so an id that is malformed, or names a mailbox outside
  the grant, is reported under `rejected` while the rest of the batch proceeds. Every id is
  still authorized individually: rejecting one is not a way past the gate, it is a way of not
  losing the other nineteen.
- **A failure carries a code**, from the same taxonomy as a whole-call error, because a
  client that cannot tell `unsupported_by_provider` from `provider_error` will retry a
  permanent refusal forever.
- **If nothing succeeded, the call is an error**, carrying the same `accounts` and `rejected`
  blocks, rather than a success that reports having done nothing.

Every entry is an object, including `mail_labels list`, which reports
`{ "labels": [ … ] }` rather than a bare array. An array has nowhere to put the name of the
mailbox its contents came from, and a listing that cannot say which mailbox it listed is the
problem this block exists to avoid.

A mailbox's entry also carries `not_recorded` when the change was made and the audit row
could not be written. That is deliberately not a failure: the mailbox has already changed, so
an error would report something that did not happen and invite a retry that does the work
twice. Reads go the other way — an audit row that cannot be written withholds the result,
because there the refusal still prevents the unrecorded read.

It is worth stating plainly rather than leaving a client to discover it: that is two response
shapes, and the rule below asking for one is where this is going rather than where it is.

### Merging

Results merge by date, newest first, with account ID as a stable tiebreak so equal timestamps
do not reorder between calls.

`limit` is a **total across all accounts**, not per account. The server fetches up to `limit`
from each, merges, truncates, and records per-account positions in the cursor. A model asking
for 20 results gets 20, not 20 × however many mailboxes happen to be linked.

### Cursors

The cursor is opaque and encodes each account's own pagination state. Accounts that are
exhausted drop out of subsequent pages; the call completes when all are exhausted.

Cursors are not valid across a grant change. If accounts are added or removed from a grant,
an old cursor is rejected rather than silently returning a different scope than the first
page did.

### Timeouts

Each account gets its own deadline. One slow provider degrades to `timeout` in the status
block; it does not hold the whole response.

## Identifiers

Message and thread IDs are namespaced with the **immutable account ID**, not the alias:

```
acct_01JB4X…:1234567890abc
```

Aliases are mutable. If IDs embedded the alias, renaming a mailbox would invalidate every ID
a model was holding mid-conversation. The alias travels alongside as a display field on each
result instead, with the address the alias currently names:

```jsonc
{ "id": "acct_01JB4X…:1234567890abc", "account": "work",
  "account_address": "you@example.com", "subject": "…" }
```

So the model reads and reasons in aliases, and round-trips IDs it never has to parse.

## Naming a mailbox back to the caller

An alias is a label somebody chose. Two people's `main` are different mailboxes, and a result
that says only `main` leaves an agent unable to say which mailbox it just read, sent from or
emptied — and leaves whoever reads the transcript afterwards no better off, which is the
moment it matters, because by then the mail has gone.

So every result that names a mailbox names its address too, and always as **two fields rather
than one label**:

| Where | Identifier | Address |
|---|---|---|
| `mail_accounts` | `alias` | `address` |
| A search row, a message, a thread | `account` | `account_address` |
| A single-mailbox result — send, draft, filters, settings | `account` | `account_address` |
| An aggregated `accounts` block | the key | `address` inside the entry |
| `scope_denied` | `account` | `account_address` |

The identifier keeps its old key and its old value. `account` is still the alias, the
`accounts` block is still keyed by alias, and both are still exactly what `account` accepts
back — so anything a caller was passing through before still resolves. Nothing renders a
combined `work - you@example.com` where a caller reads a selector from, because that string
names no mailbox.

The combined form appears in one place: the prose of an error message, which exists to be
relayed to a person and is never parsed.

```
scope_denied: this grant holds read, draft on work - you@example.com.
That action requires "send".
```

## Errors

Three distinct failures that must never be collapsed into one, because the correct response
to each is different:

| Error | Means | Model should |
|---|---|---|
| `scope_denied` | The grant lacks the capability or the account | Tell the user what to grant, and stop |
| `unsupported_by_provider` | The mailbox cannot do this at all | Stop, or try another account |
| `provider_error` | It failed this time | Retry may help |

Both `scope_denied` and `unsupported_by_provider` name the account and the capability:

```jsonc
{
  "error": "scope_denied",
  "account": "work",
  "account_address": "you@example.com",
  "capability": "send",
  "message": "scope_denied: this grant holds read, draft, labels on work - you@example.com. That action requires \"send\"."
}
```

The message is written for a model to relay to a human, because that is exactly what happens
to it.

## Tools

| Tool | Fans out | Requires |
|---|---|---|
| `mail_accounts` | — | — |
| `mail_search` | yes | `read` |
| `mail_get_message` | no | `read` |
| `mail_get_thread` | no | `read` |
| `mail_get_attachment` | no | `attachments` |
| `mail_upload_url` | no | `draft` / `send` |
| `mail_draft` | no | `draft` / `discard` |
| `mail_send` | no | `send` |
| `mail_modify` | yes | `labels`, plus `destructive` where a label bins |
| `mail_trash` | yes | `destructive` |
| `mail_labels` | yes | `read` / `labels` |
| `mail_filters` | no | `filters` |
| `mail_settings` | no | `settings` |

`mail_draft` is the second tool two capabilities reach, and the check is per action rather
than per tool: `create` and `update` need `draft`, `delete` needs `discard`. A grant holding
one and not the other is offered the tool and refused the action it may not take, which is
what lets an agent compose without being able to destroy a draft somebody wrote. Sending a
draft removes it and still needs only `send` — see [grants.md](grants.md#capabilities).

Five of these change their wording with the grant's mode, and four of them change their
behaviour under `hold`:

| Tool | Wording varies | Held under `hold` |
|---|---|---|
| `mail_accounts` | yes | — |
| `mail_draft` | yes | no, `discard` included |
| `mail_modify` | yes | **yes**, where a label bins or junks |
| `mail_send` | yes | **yes** |
| `mail_trash` | yes | **yes**, except `untrash` |
| `mail_filters` | yes | **yes**, `create` and `delete` |
| `mail_settings` | yes | **yes**, `set_vacation` |

Everything else reads the same in every mode, which is the right answer for a read and for a
reversible change: a caution repeated on every description is a caution read on none of them.

### Annotations

Every tool also carries MCP `annotations`, which are the machine-readable half: a client reads
them to decide what to auto-approve, what to put a confirmation in front of, and what is safe
to repeat after a timeout. A description is weighed; an annotation is acted on.

| Tool | `readOnlyHint` | `destructiveHint` | `idempotentHint` | `openWorldHint` |
|---|---|---|---|---|
| `mail_accounts` | yes | no | yes | yes |
| `mail_search` | yes | no | yes | yes |
| `mail_get_message` | yes | no | yes | yes |
| `mail_get_thread` | yes | no | yes | yes |
| `mail_get_attachment` | no | no | no | yes |
| `mail_upload_url` | no | no | no | **no** |
| `mail_labels` | no | yes | no | yes |
| `mail_draft` | no | yes | no | yes |
| `mail_send` | no | yes | no | yes |
| `mail_modify` | no | yes | no | yes |
| `mail_trash` | no | yes | no | yes |
| `mail_filters` | no | yes | no | yes |
| `mail_settings` | no | yes | no | yes |

Four rows are worth explaining.

`mail_get_attachment` is not read-only. It takes nothing out of the mailbox, but it copies the
attachment onto this server, charges it to the owner's storage allowance and mints a URL that
serves the file to anyone holding it with no token. A client that called it freely on the
strength of a read-only hint would fill the allowance and leave live credentials behind it.
The `inline` path really is a read; an annotation covers a tool rather than an argument.

`mail_upload_url` is the only closed world here. It reaches no provider and no mailbox — it
reserves space in mailroom's own store and signs a URL for it.

`mail_modify` reads as though it should be the one idempotent write — every field on it is a
state to put a message into rather than an increment — and it is not, because part of it bins
mail. See [labels that destroy mail](#labels-that-destroy-mail): applying the bin label is
`destructive` on this tool too, and under `hold` it becomes a queued action rather than a
change, so a client repeating a call that timed out would put a second binning in front of the
mailbox's owner.

`mail_send` is marked destructive, which stretches the word deliberately. Sending adds rather
than removes, so a literal reading argues for `false` — but `false` is the positive claim
"this only ever adds", and this is the one call whose result cannot be taken back.

**Annotations do not vary with the mode, although descriptions do.** Under `hold` a send
reaches no provider, and it would be easy to annotate that connection's `mail_send` as
harmless. Mode-dependent steering makes a description more cautious; mode-dependent
annotations would make the flags less cautious, and they are the half a client acts on without
judgement. A tool list can be cached at connection time and a grant's mode can be edited
afterwards, so a hint that says "safe" under `hold` is one that outlives the mode it was true
for. `hold` also never covers a whole tool: `untrash`, listing filters and reading settings all
run immediately. The reasoning is in `internal/mcp/annotations.go`.

Tools taking an explicit ID do not fan out — the ID already names its account. Neither do the
administrative tools: applying a filter or an auto-reply to every mailbox a grant happens to
cover, because the caller omitted a name, is not a mistake worth allowing. They ask for one
mailbox and refuse to guess.

## The administrative tail

`mail_filters` and `mail_settings` take an `action` rather than splitting into a tool each.
They are touched rarely, and a dozen more tool definitions would cost every client context on
every call for the sake of a quarterly operation.

**Filter actions are label changes**, not named operations: archiving is removing `INBOX`,
trashing is adding `TRASH`. Modelling those as their own flags would bake Gmail's vocabulary
into the contract, the same reason `mail_modify` takes labels rather than an archive flag.

That equivalence cuts both ways, which is what
[destructive labels](#labels-that-destroy-mail) is about: a filter whose `add_labels` names the
bin is a standing instruction to delete mail, so it needs `destructive` as well as `filters`.

Read state and the star are the exception, and they go the other way. Gmail keeps both as
labels named `UNREAD` and `STARRED`; nobody else does, so `mail_modify`'s `read` and `starred`
travel as a flag update rather than as label ids. Both are optional and independent: leaving
one out leaves it alone, rather than setting it to false. See
[providers.md](providers.md#read-state-and-stars-are-not-labels).

### Labels that destroy mail

Most of `mail_modify` is filing, and filing is what `labels` is for. One part of it is not.
Applying a label is how every provider here bins a message: Gmail's `BatchModify` moves the
mail when `TRASH` is added, and on IMAP, Zoho and Graph applying an exclusive label *is* a
move, so naming the bin folder sends the same request `mail_trash` does. That was reachable
with `labels` alone, on a grant in `hold`, and neither the capability nor the mode noticed.

So the rule is on the **effect** rather than on the tool: a label operation whose effect is
destruction needs `destructive` as well as `labels`, and is held exactly as `mail_trash` is.
It applies wherever a label is applied — `mail_modify`, and a `mail_filters` rule whose
`add_labels` names the bin.

Three places enforce it, and the split is the point:

- **The provider classifies.** `LabelManager.EffectOfApplying` is on the interface, so the
  compiler asks every provider which of its own ids is the bin. Matching provider strings in
  the MCP layer instead would leave the next provider's bin ordinary until somebody
  remembered — which is how the hole appeared in the first place. See
  [providers.md](providers.md#some-labels-are-not-filing).
- **The tool decides.** Only a handler can turn a destructive change into a queued action with
  a summary its owner can read, so the `hold` arrangement lives where the result is built. A
  batch spanning two mailboxes is classified before any of them is touched, and refused whole
  rather than performed where it happened to be harmless.
- **The provider boundary refuses.** `labelManager` hands the tools a guarded manager rather
  than the provider's own, so a label change reaching a provider that nobody authorized is
  refused rather than performed. That is the part that survives the next tool somebody writes.

A classification that fails — Graph unreachable while resolving a folder id — refuses the
call. A check that could not be made has not passed.

**`mail_settings` reads everything and writes one thing.** Only the vacation responder can be
changed. Forwarding and delegation hand somebody else access to the mail itself, which is a
decision for a person at a settings page rather than something an agent should arrange — so
they stay readable and no more.

Sections a provider cannot serve return `unsupported_by_provider` rather than an empty
answer, and that includes cases where the deployment or the mailbox is the limit rather than
the provider. `delegates` is unsupported on every Gmail account, because it needs the
`gmail.settings.sharing` scope and mailroom does not request it; on a consumer account it
would be refused anyway, delegation being a Workspace feature. Either way the answer is a
refusal rather than an empty delegate list that reads as "nobody is delegated".

## Attachments

**No attachment's bytes travel through the conversation in either direction.** Both ways
across are the same shape: mailroom hands out a short-lived signed URL, and the client makes
its own HTTP request to it.

That is not an optimisation. A 5 MB PDF base64'd into a tool result is about 6.7 MB of context
spent on something a model cannot read from its own transcript anyway; and in the other
direction the MCP transport caps a request at 4 MiB, so a file worth sending does not fit at
all. MCP also gives this server no access to the client's filesystem, which is why an upload
has to be a URL the client itself writes to — there is no other way an agent's local file can
get here.

### Downloading

`mail_get_attachment` answers with a link, not a file:

```jsonc
{
  "url": "https://mail.example.com/attachments/v1.d.blob_01JB…",
  "filename": "invoice.pdf",
  "mime_type": "application/pdf",
  "size_bytes": 5242880,
  "expires_at": "2026-08-20T09:15:00Z"
}
```

The same URL is also returned as an MCP `resource_link`, for a client that can act on one
directly. Fetch it with an ordinary GET and no `Authorization` header: the signature in the
path is the whole credential.

`inline: true` is the exception, for a small text file whose wording is the point — a note, a
short CSV. It is capped at 64 KiB and refuses anything that is not valid text, pointing back
at the link. It never returns base64: base64 in a conversation defeats the only reason to put
content there.

MCP promises nothing about a client being able to make its own HTTP requests, and this tool
assumes it can. An agent that cannot has a correct move and the description names it: give the
url to the person it is working with, along with the filename and the expiry. That is a
finished job rather than a failure, and it has to be said rather than inferred — otherwise the
agent reports that the file could not be retrieved.

### Uploading

`mail_upload_url` mints somewhere to put a file:

```jsonc
{
  "upload_url": "https://mail.example.com/attachments/upload/v1.u.blob_01JB…",
  "method": "PUT",
  "blob_id": "blob_01JB…",
  "max_bytes": 18874368,
  "expires_at": "2026-08-20T09:10:00Z"
}
```

PUT the file's bytes as the raw request body, with no `Authorization` header. The URL works
**once** — the bytes behind a `blob_id` a caller already holds must not be replaceable after
the fact — and a second PUT gets `409`.

The same gap applies here and is worse, because half a workflow leaves a `blob_id` that names
nothing. A client with no HTTP of its own either attaches the file inline as `content_base64`,
under the 2 MiB cap, or hands `upload_url` to a person and waits for them to confirm the PUT
before naming the `blob_id`.

### Attaching

`mail_draft` and `mail_send` take an `attachments` list, and each entry names exactly one of
three sources. Naming two is refused rather than resolved.

```jsonc
{ "from_message": "acct_01JB4X…:1234567890abc", "attachment_id": "ANGjdJ8…" }
{ "blob_id": "blob_01JB…" }
{ "filename": "summary.csv", "mime_type": "text/csv", "content_base64": "aWQsbmFtZQo…" }
```

**By reference** reuses something already in a mailbox: mailroom reads those bytes out of the
source mailbox and writes them into the new message entirely server-side. Forwarding an
invoice you were sent is the common case, and this is the way to do it.

**By blob** names an upload. This is the path for a file the client holds locally, or anything
over about 2 MiB.

**Inline** is for genuinely new, genuinely small content that came from neither.

`filename` is required inline and optional otherwise, where it overrides the original name.
`mime_type` defaults to `application/octet-stream` inline, or to the source's type.

### Limits

| | Limit | Why |
|---|---|---|
| One inline attachment | 2 MiB | It travels inside the MCP request, which the transport caps at 4 MiB. The rest of the call needs the remaining room. |
| One upload | 18 MiB | The same ceiling as a whole message; declaring a smaller `size_bytes` narrows it further. |
| All attachments on a message | 18 MiB | Base64 inflates by 4/3, so 18 MiB encodes to roughly 24.6 MB — under Gmail's 25 MB ceiling. |
| One inline *download* | 64 KiB | The one path that still puts content in the caller's context. |

The upload ceiling and the per-message total are the same constant rather than two numbers
that agree today. An upload larger than a message could carry would be accepted, stored, and
then refused at send, having already spent the disk.

An upload's limit is enforced while the body is being written rather than read off
`Content-Length`, which a client can get wrong and a chunked body does not carry at all. Going
over aborts the write, deletes the partial blob and answers `413`.

### Permission

A referenced attachment is a **read of whichever mailbox holds it**, which need not be the
mailbox being composed in. That read is authorized on its own terms: the grant must cover the
source account and hold `attachments` on it. Holding an id proves nothing — ids appear in
search results and get quoted in conversation — so each one goes through the gate exactly as
`mail_get_attachment` does, and the account comes from the id itself rather than from anything
the caller says alongside it.

So attaching across mailboxes needs `draft` or `send` on the destination *and* `attachments`
on the source. A grant holding only one of those cannot move a file between two mailboxes.

`mail_get_attachment` requires `attachments` rather than `read`. Reading a message body and
pulling down a contract or a spreadsheet of customer records are different risks, and they get
different checkboxes.

`mail_upload_url` requires `draft` or `send`, not `attachments`. `attachments` means reading
files out of mail; minting an upload URL means writing bytes to this server's disk, and the
only thing those bytes can ever be used for is being attached to a message. So the permission
that reaches them is the one that could attach them.

### What a link is worth

A signed URL is a credential, and it is narrow in every direction. It names one blob, one
owner and one grant; it is signed with a key derived from `MAILROOM_ENCRYPTION_KEY` for this
purpose and no other; a download link expires with the bytes it points at, and an upload URL
sooner still.

**Revocation beats the signature.** Every request under `/attachments/` re-reads the grant that
minted the link, so revoking a grant — or editing it to drop the mailbox or the capability —
kills its outstanding links at once rather than at their own expiry. A link that outlived a
revocation would be the one place in this server where pressing Revoke did not stop a client
reading the mail, and it is the sort of credential that ends up sitting in a transcript.

### When one stops working

Every refusal on these paths names the call that produces a working link, because "expired" on
its own leaves an agent with nowhere to go and no way to tell a dead link from a deleted file.

| What happened | Status | What the message says |
|---|---|---|
| Download link expired | `410` | The attachment is still in the mailbox; call `mail_get_attachment` again |
| Grant revoked or narrowed | `403` | The access changed, the file has not moved; ask for it back, then mint a new link |
| Upload URL expired | `410` | Nothing was written through it; call `mail_upload_url` again |
| Upload URL already used | `409` | One PUT each, no overwrite or resume; mint another |
| Body over the ceiling | `413` | The ceiling, and that it was refused mid-write so the same file will fail again |
| Nothing PUT yet | `404` | The reservation exists and the upload is what is missing |
| `blob_id` past its TTL | tool error | Staged uploads are deleted; upload again and name the new id |

The last one is a tool result rather than an HTTP status, because it surfaces when `mail_draft`
or `mail_send` names a `blob_id`. Once a blob has been swept, an expired id and an id that
never existed are the same absence, so the message leads with the likelier of the two instead
of reporting "not found".

A response from `/attachments/` is always `Content-Disposition: attachment`, always `nosniff`,
always `no-store`, and never carries a content type a caller chose: anything not on a list of
types no browser executes is served as `application/octet-stream`. An uploaded HTML or SVG file
fetched back is an inert download rather than script running in the origin that holds the
operator's session.

## Prompts

One, `mail_attachments`, and the test for whether a prompt earns its place is whether it
explains something no single tool owns. The attachment workflow does: it spans three steps and
only two of them are tool calls, because the HTTP request in the middle is the client's own. A
model that misses that step holds a `blob_id` pointing at an empty reservation and cannot tell
why the attachment is missing — and no tool description has the sequence in scope.

It is offered only to a grant holding `attachments`, `draft` or `send`, and each half is cut to
what that grant can call, for the same reason tools are filtered: instructions for a call a
client cannot make are context spent on nothing.

Nothing else here gets a prompt. Searching, reading and sending are each one call and their
descriptions already say when to make it; a second prompt restating them would make this one
harder to find.

## Rules for adding a tool

1. Take an `account` parameter unless the operation is inherently single-account.
2. Return the aggregated envelope if it can fan out, even when only one account is in scope —
   a client should never have to handle two response shapes for the same tool.
3. Name the required capability in the tool description, so a model can reason about its own
   scope before calling.
4. Wherever the result names a mailbox, name its address beside the alias — as a separate
   field, never folded into one label.
5. Never collapse the three error kinds.
6. Give the tool a row in `toolAnnotations`. A tool with no row ships as
   destructive-and-open-world by default rather than by decision, and the tests fail.
7. Write every refusal as what to do next. "This link has expired" is a state; "call
   `mail_upload_url` again for a fresh URL and `blob_id`" is an instruction, and an agent
   reading the first one has nowhere to go.
