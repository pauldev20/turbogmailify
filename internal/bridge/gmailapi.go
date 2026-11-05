package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

// API is the subset of Gmail calls the bridge uses.
type API interface {
	Import(raw []byte, labels []string, neverMarkSpam, processForCalendar bool) (*gmail.Message, error)
	Insert(raw []byte, labels []string, threadID string) (*gmail.Message, error)
	Get(id string) (*gmail.Message, error) // format=raw
	Labels(id string) ([]string, error)
	AddLabels(id string, labels []string) error
	Trash(id string) error
	Delete(id string) error
	Search(query string) ([]string, error)
	History(startID uint64, pageToken string) (*gmail.ListHistoryResponse, error)
	HistoryID() (uint64, error)
	LabelID(name string) (string, error) // creates the label if missing
}

// GmailAPI implements API over the Gmail REST service.
type GmailAPI struct{ S *gmail.Service }

func (g GmailAPI) Import(raw []byte, labels []string, neverMarkSpam, processForCalendar bool) (*gmail.Message, error) {
	return g.S.Users.Messages.
		Import("me", &gmail.Message{LabelIds: labels}).
		InternalDateSource("dateHeader").
		NeverMarkSpam(neverMarkSpam).
		ProcessForCalendar(processForCalendar).
		Deleted(false).
		Media(bytes.NewReader(raw), googleapi.ContentType("message/rfc822")).
		Do()
}

func (g GmailAPI) Insert(raw []byte, labels []string, threadID string) (*gmail.Message, error) {
	return g.S.Users.Messages.
		Insert("me", &gmail.Message{LabelIds: labels, ThreadId: threadID}).
		InternalDateSource("dateHeader").
		Media(bytes.NewReader(raw), googleapi.ContentType("message/rfc822")).
		Do()
}

func (g GmailAPI) Get(id string) (*gmail.Message, error) {
	return g.S.Users.Messages.Get("me", id).Format("raw").Do()
}

func (g GmailAPI) Labels(id string) ([]string, error) {
	m, err := g.S.Users.Messages.Get("me", id).Format("minimal").Do()
	if err != nil {
		return nil, err
	}
	return m.LabelIds, nil
}

func (g GmailAPI) AddLabels(id string, labels []string) error {
	_, err := g.S.Users.Messages.Modify("me", id, &gmail.ModifyMessageRequest{AddLabelIds: labels}).Do()
	return err
}

func (g GmailAPI) Trash(id string) error {
	_, err := g.S.Users.Messages.Trash("me", id).Do()
	return err
}

func (g GmailAPI) Delete(id string) error {
	return g.S.Users.Messages.Delete("me", id).Do()
}

func (g GmailAPI) Search(query string) ([]string, error) {
	var ids []string
	err := g.S.Users.Messages.List("me").Q(query).Pages(context.Background(), func(r *gmail.ListMessagesResponse) error {
		for _, m := range r.Messages {
			ids = append(ids, m.Id)
		}
		return nil
	})
	return ids, err
}

func (g GmailAPI) History(startID uint64, pageToken string) (*gmail.ListHistoryResponse, error) {
	return g.S.Users.History.List("me").
		StartHistoryId(startID).
		LabelId("SENT").
		HistoryTypes("messageAdded", "labelAdded").
		PageToken(pageToken).
		Do()
}

func (g GmailAPI) HistoryID() (uint64, error) {
	p, err := g.S.Users.GetProfile("me").Do()
	if err != nil {
		return 0, err
	}
	return p.HistoryId, nil
}

func (g GmailAPI) LabelID(name string) (string, error) {
	r, err := g.S.Users.Labels.List("me").Do()
	if err != nil {
		return "", err
	}
	for _, l := range r.Labels {
		if l.Name == name {
			return l.Id, nil
		}
	}
	l, err := g.S.Users.Labels.Create("me", &gmail.Label{
		Name:                  name,
		LabelListVisibility:   "labelShow",
		MessageListVisibility: "show",
	}).Do()
	if err != nil {
		return "", fmt.Errorf("create label %q: %w", name, err)
	}
	return l.Id, nil
}

func isNotFound(err error) bool {
	gerr, ok := errors.AsType[*googleapi.Error](err)
	return ok && gerr.Code == http.StatusNotFound
}
