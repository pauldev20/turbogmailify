package mocks

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

// GmailMessage is one message in FakeGmail.
type GmailMessage struct {
	Raw      []byte
	Labels   []string
	ThreadID string
	Trashed  bool
	Deleted  bool
}

// FakeGmail is an in-memory implementation of bridge.API.
type FakeGmail struct {
	Messages map[string]*GmailMessage
	Calls    []string // "import", "insert", "trash:<id>", ...

	ImportLabels   []string // extra labels Gmail "assigns" on import, e.g. SPAM
	ImportNoLabels bool     // Import response omits labelIds
	InsertErr      error
	InsertErrs     []error // returned one per call before InsertErr; nil succeeds
	InsertLost     []error // like InsertErrs, but the message is stored anyway
	IndexLag       bool    // messages stored by InsertLost stay out of Search until Reindex
	unindexed      map[string]bool
	GetErrs        []error // returned one per call before succeeding
	TrashErr       error
	DeleteErr      error
	SearchErr      error
	LabelsErr      error

	Events        []*gmail.History
	HistoryIDNow  uint64
	HistoryExpire bool // History returns 404
	Labelled      map[string][]string

	nextID int
}

func NewFakeGmail() *FakeGmail {
	return &FakeGmail{Messages: map[string]*GmailMessage{}, Labelled: map[string][]string{}}
}

// Add stores a message and returns its id.
func (f *FakeGmail) Add(raw []byte, labels []string, threadID string) string {
	f.nextID++
	id := fmt.Sprintf("m%d", f.nextID)
	if threadID == "" {
		threadID = "t" + id
	}
	f.Messages[id] = &GmailMessage{Raw: raw, Labels: labels, ThreadID: threadID}
	return id
}

func (f *FakeGmail) msg(id string) *gmail.Message {
	m := f.Messages[id]
	return &gmail.Message{Id: id, ThreadId: m.ThreadID, LabelIds: slices.Clone(m.Labels)}
}

func (f *FakeGmail) Import(raw []byte, labels []string, _, _ bool) (*gmail.Message, error) {
	f.Calls = append(f.Calls, "import")
	m := f.msg(f.Add(raw, slices.Concat(labels, f.ImportLabels), ""))
	if f.ImportNoLabels {
		m.LabelIds = nil
	}
	return m, nil
}

func (f *FakeGmail) Insert(raw []byte, labels []string, threadID string) (*gmail.Message, error) {
	f.Calls = append(f.Calls, "insert")
	if len(f.InsertLost) > 0 {
		err := f.InsertLost[0]
		f.InsertLost = f.InsertLost[1:]
		id := f.Add(raw, labels, threadID)
		if f.IndexLag {
			if f.unindexed == nil {
				f.unindexed = map[string]bool{}
			}
			f.unindexed[id] = true
		}
		return nil, err
	}
	if len(f.InsertErrs) > 0 {
		err := f.InsertErrs[0]
		f.InsertErrs = f.InsertErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if f.InsertErr != nil {
		return nil, f.InsertErr
	}
	return f.msg(f.Add(raw, labels, threadID)), nil
}

func (f *FakeGmail) Get(id string) (*gmail.Message, error) {
	if len(f.GetErrs) > 0 {
		err := f.GetErrs[0]
		f.GetErrs = f.GetErrs[1:]
		return nil, err
	}
	m, ok := f.Messages[id]
	if !ok || m.Deleted {
		return nil, &googleapi.Error{Code: 404}
	}
	g := f.msg(id)
	g.Raw = base64.URLEncoding.EncodeToString(m.Raw)
	return g, nil
}

func (f *FakeGmail) Labels(id string) ([]string, error) {
	if f.LabelsErr != nil {
		return nil, f.LabelsErr
	}
	return f.msg(id).LabelIds, nil
}

func (f *FakeGmail) AddLabels(id string, labels []string) error {
	f.Labelled[id] = append(f.Labelled[id], labels...)
	return nil
}

func (f *FakeGmail) Trash(id string) error {
	f.Calls = append(f.Calls, "trash:"+id)
	if f.TrashErr != nil {
		return f.TrashErr
	}
	f.Messages[id].Trashed = true
	return nil
}

func (f *FakeGmail) Delete(id string) error {
	f.Calls = append(f.Calls, "delete:"+id)
	if f.DeleteErr != nil {
		return f.DeleteErr
	}
	f.Messages[id].Deleted = true
	return nil
}

// Search supports "rfc822msgid:<id>" and "label:<id>" (other terms are
// ignored), the latter newest first by Date header like Gmail; any other query returns every SENT message
// that is not trashed.
func (f *FakeGmail) Search(q string) ([]string, error) {
	if f.SearchErr != nil {
		return nil, f.SearchErr
	}
	for _, term := range strings.Fields(q) {
		if label, ok := strings.CutPrefix(term, "label:"); ok {
			return f.searchLabel(label), nil // other terms are ignored
		}
	}
	var ids []string
	for id, m := range f.Messages {
		if m.Trashed || m.Deleted || f.unindexed[id] {
			continue
		}
		if want, ok := strings.CutPrefix(q, "rfc822msgid:"); ok {
			if msg, err := mail.ReadMessage(bytes.NewReader(m.Raw)); err == nil &&
				msgID(msg.Header.Get("Message-ID")) == want {
				ids = append(ids, id)
			}
		} else if slices.Contains(m.Labels, "SENT") {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// Reindex makes every stored message searchable.
func (f *FakeGmail) Reindex() { f.unindexed = nil }

// msgID returns the id inside the angle brackets of a Message-ID value, as
// Gmail indexes it.
func msgID(v string) string {
	if _, rest, ok := strings.Cut(v, "<"); ok {
		v, _, _ = strings.Cut(rest, ">")
	}
	return strings.TrimSpace(v)
}

func (f *FakeGmail) searchLabel(label string) []string {
	var ids []string
	for id, m := range f.Messages {
		if !m.Trashed && !m.Deleted && slices.Contains(m.Labels, label) {
			ids = append(ids, id)
		}
	}
	date := func(id string) time.Time {
		msg, err := mail.ReadMessage(bytes.NewReader(f.Messages[id].Raw))
		if err != nil {
			return time.Time{}
		}
		d, _ := msg.Header.Date()
		return d
	}
	num := func(id string) int { n, _ := strconv.Atoi(strings.TrimPrefix(id, "m")); return n }
	slices.SortFunc(ids, func(a, b string) int {
		if c := date(b).Compare(date(a)); c != 0 {
			return c
		}
		return num(b) - num(a)
	})
	return ids
}

func (f *FakeGmail) History(startID uint64, _ string) (*gmail.ListHistoryResponse, error) {
	if f.HistoryExpire {
		return nil, &googleapi.Error{Code: 404}
	}
	var h []*gmail.History
	for _, e := range f.Events {
		if e.Id > startID {
			h = append(h, e)
		}
	}
	return &gmail.ListHistoryResponse{History: h, HistoryId: f.HistoryIDNow}, nil
}

// AddSent stores a SENT message and appends a matching history record.
func (f *FakeGmail) AddSent(raw []byte, labels ...string) string {
	id := f.Add(raw, append([]string{"SENT"}, labels...), "")
	f.HistoryIDNow++
	f.Events = append(f.Events, &gmail.History{
		Id:            f.HistoryIDNow,
		MessagesAdded: []*gmail.HistoryMessageAdded{{Message: &gmail.Message{Id: id}}},
	})
	return id
}

func (f *FakeGmail) HistoryID() (uint64, error) { return f.HistoryIDNow, nil }

func (f *FakeGmail) LabelID(name string) (string, error) { return "L:" + name, nil }
