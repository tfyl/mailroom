package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/ids"
	"github.com/tfyl/mailroom/internal/mail"
	imapprovider "github.com/tfyl/mailroom/internal/provider/imap"
	"github.com/tfyl/mailroom/internal/secrets"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// runLinkIMAP attaches an IMAP mailbox from the command line.
//
// It exists so that a deployment can be finished without a browser. Gmail and Zoho are linked
// by an OAuth round trip that only a person at a keyboard can complete, and that is
// unavoidable for them — but IMAP authenticates with a password, and a mailbox reachable that
// way should not require somebody to click through a web form to attach it. With an app
// password this is the only path to a working mailbox that touches no OAuth client, no
// consent screen and no Console at all.
//
// The password never arrives as a flag. Process arguments are readable by every user on the
// machine through ps and land in shell history, and a mailbox password is not something to
// leave in either.
func runLinkIMAP(args []string) error {
	flags := flag.NewFlagSet("link-imap", flag.ContinueOnError)
	var (
		alias    = flags.String("alias", "", "short name you will refer to this mailbox by. Permanent, and never reused")
		address  = flags.String("address", "", "the email address, used for display and as the default sender")
		host     = flags.String("imap", "", "IMAP server as host:port, such as imap.gmail.com:993")
		username = flags.String("username", "", "IMAP username; usually the address (defaults to -address)")
		smtpHost = flags.String("smtp", "", "SMTP server as host:port for sending. Omit to link a mailbox that cannot send")
		smtpFrom = flags.String("smtp-from", "", "envelope sender, if it differs from -address")
		noTLS    = flags.Bool("insecure", false, "connect to IMAP without TLS. Only sane against localhost")
		owner    = flags.String("owner", "", "user id that will own this mailbox (defaults to the account that owns the instance)")
		skip     = flags.Bool("skip-verify", false, "store the credentials without checking they work")
	)
	flags.Usage = func() {
		fmt.Fprint(flags.Output(), `usage: mailroom link-imap -alias NAME -address ADDR -imap HOST:PORT [flags]

The password is read from MAILROOM_LINK_PASSWORD, or from standard input if that
is unset. It is never taken as a flag: arguments are visible to every process on
the machine and are kept in shell history.

For Gmail, the password is a 16-character app password, which needs 2-Step
Verification switched on. It is not your account password, and it is the only
way to reach Gmail without an OAuth client.

  MAILROOM_LINK_PASSWORD=xxxx mailroom link-imap \
    -alias personal -address you@gmail.com \
    -imap imap.gmail.com:993 -smtp smtp.gmail.com:587

`)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}

	if *alias == "" {
		return errors.New("-alias is required: it is how grants and tools will refer to this mailbox")
	}
	parsedAlias, err := mail.ParseAlias(*alias)
	if err != nil {
		return err
	}
	*alias = parsedAlias

	switch {
	case *address == "":
		return errors.New("-address is required")
	case *host == "":
		return errors.New("-imap is required, as host:port")
	}

	password, err := readPassword()
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	sealer, err := secrets.NewSealer(cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("encryption key: %w", err)
	}
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	ownerID, err := resolveOwner(ctx, db, *owner)
	if err != nil {
		return err
	}

	account := mail.Account{
		ID:       mail.AccountID(ids.Account()),
		Alias:    *alias,
		Address:  *address,
		Provider: mail.ProviderIMAP,
		Status:   mail.StatusLinked,
	}
	imapCfg := imapprovider.Config{
		Host:     *host,
		Username: firstNonEmpty(*username, *address),
		Password: password,
		TLS:      !*noTLS,
		SMTPHost: *smtpHost,
		SMTPFrom: firstNonEmpty(*smtpFrom, *address),
	}

	// Connecting before storing turns a typo into an error now rather than a mailbox that
	// exists, looks linked, and fails on first use. The check is skippable for a server that
	// is not reachable from wherever this command is being run.
	if !*skip {
		provider, err := imapprovider.New(ctx, account, imapCfg)
		switch {
		case errors.Is(err, mail.ErrNeedsReauth):
			// The provider reports a rejected login as "re-link required", which is the
			// right words for a mailbox that was working and stopped. Here there is nothing
			// to re-link: this is the first attempt and the credentials are simply wrong.
			return fmt.Errorf("%s rejected the credentials for %s.\n"+
				"For Gmail this is a 16-character app password, which needs 2-Step Verification "+
				"switched on. It is not your account password", imapCfg.Host, imapCfg.Username)
		case err != nil:
			return fmt.Errorf("could not reach %s: %w", imapCfg.Host, err)
		}
		_ = provider.Close()
	}

	blob, err := json.Marshal(imapCfg)
	if err != nil {
		return err
	}
	sealed, err := sealer.SealString(string(blob), string(account.ID))
	if err != nil {
		return fmt.Errorf("could not seal the credential: %w", err)
	}
	if err := db.LinkAccount(ctx, ownerID, account, sealed, ""); err != nil {
		return err
	}

	fmt.Println(account.ID)
	fmt.Fprintf(os.Stderr, "Linked %s as %q.%s\n", *address, *alias,
		map[bool]string{true: "", false: " Sending is disabled: no -smtp was given."}[*smtpHost != ""])
	return nil
}

// readPassword takes the password from the environment, or from stdin when it is not set.
//
// Two sources rather than one because they serve different callers: an environment variable
// suits an unattended deployment where the value comes from a secret store, and standard
// input suits a person at a terminal piping from a password manager.
//
// A person typing rather than piping gets the third case: echo off, so the password is not
// left on the screen and in the scrollback of whoever walks past next.
func readPassword() (string, error) {
	if v := os.Getenv("MAILROOM_LINK_PASSWORD"); v != "" {
		return v, nil
	}

	var raw []byte
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "Password (or set MAILROOM_LINK_PASSWORD): ")
		typed, err := term.ReadPassword(fd)
		// The Enter that ended the line was not echoed either, so nothing else printed here
		// would start on a line of its own.
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		raw = typed
	} else {
		piped, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		raw = piped
	}

	// Trimmed because an app password is usually pasted with the spaces Google displays it
	// with, and a trailing newline arrives from every pipe.
	password := strings.Join(strings.Fields(string(raw)), "")
	if password == "" {
		return "", errors.New("no password given: set MAILROOM_LINK_PASSWORD or pipe it on standard input")
	}
	return password, nil
}

// resolveOwner picks the user this mailbox belongs to.
//
// Defaulting to the account that owns the instance is right for the single-operator case this
// command exists to serve, and wrong to guess at on a shared instance — so naming somebody is
// possible and being explicit is never punished.
func resolveOwner(ctx context.Context, db *store.Store, want string) (user.ID, error) {
	users, err := db.ListUsers(ctx)
	if err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", errors.New("this instance has no accounts yet, so a mailbox would have no owner. " +
			"Sign in once first: the first sign-in claims the instance")
	}
	if want == "" {
		return users[0].ID, nil
	}
	for _, u := range users {
		if string(u.ID) == want {
			return u.ID, nil
		}
	}
	return "", fmt.Errorf("no user has the id %q", want)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
