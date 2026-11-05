package bridge

import (
	"errors"
	"fmt"
	"log"
	"slices"

	"github.com/emersion/go-message/mail"
)

// Inbox implements turbogmailify's gmailInbox for one bridged account.
// DoImport only succeeds once the rewritten copy is stored, so the IMAP loop
// never expunges a message that is missing from Gmail.
type Inbox struct {
	Next Importer // plain importer, used for notifications
	b    *Bridge
	acct *account
}

// Labels that must not be copied onto the rewritten message. A SENT label
// would hand the copy to the outbound relay.
var droppedLabels = []string{"SENT", "DRAFT"}

func (in *Inbox) DoImport(envelope []byte, neverMarkSpam, processForCalendar bool, labels ...string) error {
	b := in.b
	rewritten, err := rewrite(envelope, b.cfg.RewriteMode, func(a *mail.Address) (*mail.Address, error) {
		return b.tokenize(in.acct, a)
	})
	if errors.Is(err, errMalformed) || errors.Is(err, ErrTokenCollision) {
		return fmt.Errorf("%w: rewrite: %v", ErrPermanent, err)
	}
	if err != nil {
		return err // e.g. state db unavailable; retry next cycle
	}

	// Import the unmodified message so Gmail classifies the real sender.
	orig, err := b.api.Import(envelope, slices.Concat(labels, []string{"UNREAD"}), neverMarkSpam, processForCalendar)
	if err != nil {
		return err
	}
	// The outbound relay must never treat inbound mail as a reply, even if
	// Gmail labels it SENT.
	if err := b.record(orig.Id, stateIgnored, ""); err != nil {
		in.discardOriginal(orig.Id)
		return err
	}
	labelIDs := orig.LabelIds
	if len(labelIDs) == 0 {
		if labelIDs, err = b.api.Labels(orig.Id); err != nil {
			in.discardOriginal(orig.Id)
			return err
		}
	}
	labelIDs = slices.DeleteFunc(slices.Clone(labelIDs), func(l string) bool {
		return slices.Contains(droppedLabels, l)
	})
	if in.acct.GmailLabel != "" && !slices.Contains(labelIDs, in.acct.GmailLabel) {
		labelIDs = append(labelIDs, in.acct.GmailLabel)
	}

	inserted, err := b.api.Insert(rewritten, labelIDs, orig.ThreadId)
	if err != nil {
		// Remove the original so the retry does not leave a duplicate.
		in.discardOriginal(orig.Id)
		return err
	}
	if err := b.record(inserted.Id, stateIgnored, ""); err != nil {
		log.Println("Bridge:", err)
	}
	// The rewritten copy is stored, so the source may be expunged. Failing here
	// would re-import on every cycle; tell the user instead.
	if err := b.discard(orig.Id); err != nil {
		log.Printf("Bridge: failed to discard original %s: %v", orig.Id, err)
		b.notify("Bridge: original message left in Gmail",
			fmt.Sprintf("Gmail message %s could not be removed: %v\n\nIt still carries the real reply addresses. Delete it by hand; replying to it would send from Gmail.", orig.Id, err))
	}
	return nil
}

func (in *Inbox) discardOriginal(id string) {
	if err := in.b.discard(id); err != nil {
		log.Printf("Bridge: failed to discard original %s: %v", id, err)
	}
}
