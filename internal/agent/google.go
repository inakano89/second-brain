package agent

import (
	"context"
	"net/mail"
	"strings"
	"time"
)

// Google services (GOOGLE_SERVICES keys).
const (
	GoogleCalendar = "calendar"
	GoogleGmail    = "gmail"
	GoogleDrafts   = "drafts"
	GoogleDrive    = "drive"
	GoogleContacts = "contacts"
	GoogleTasks    = "tasks"
	GoogleYouTube  = "youtube"
)

// GoogleAPI is implemented by the Google integration.
type GoogleAPI interface {
	// Can reports whether service is enabled, connected and authorised.
	Can(service string) bool

	ListEvents(ctx context.Context, from, to time.Time) ([]CalendarEvent, error)
	CreateEvent(ctx context.Context, ev CalendarEvent) (*CalendarEvent, error)

	SearchEmail(ctx context.Context, query string, max int) ([]EmailInfo, error)
	ReadEmail(ctx context.Context, id string) (*EmailInfo, error)
	CreateDraft(ctx context.Context, d EmailDraft) (*DraftResult, error)

	SearchDrive(ctx context.Context, query string, max int) ([]DriveFile, error)
	ReadDriveFile(ctx context.Context, id string) (*DriveFile, string, error)

	SearchContacts(ctx context.Context, query string, max int) ([]Contact, error)
}

// EmailInfo is a simplified Gmail message.
type EmailInfo struct {
	ID       string    `json:"id"`
	ThreadID string    `json:"thread_id,omitempty"`
	From     string    `json:"from"`
	To       string    `json:"to,omitempty"`
	Subject  string    `json:"subject"`
	Date     time.Time `json:"date"`
	Snippet  string    `json:"snippet,omitempty"`
	Body     string    `json:"body,omitempty"`
	Link     string    `json:"link"`
}

// EmailDraft is a message to be saved as a Gmail draft (never sent).
type EmailDraft struct {
	To      string `json:"to"`
	Cc      string `json:"cc,omitempty"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"` // Gmail message ID being answered
}

// DraftResult identifies a created draft.
type DraftResult struct {
	ID       string `json:"id"`
	ThreadID string `json:"thread_id,omitempty"`
	To       string `json:"to,omitempty"`
	Subject  string `json:"subject"`
	Link     string `json:"link"`
}

// DriveFile is Google Drive file metadata.
type DriveFile struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	MIME     string    `json:"mime"`
	Link     string    `json:"link,omitempty"`
	Owner    string    `json:"owner,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Created  time.Time `json:"created"`
	Modified time.Time `json:"modified"`
}

// Contact is a Google Contacts entry.
type Contact struct {
	Resource string   `json:"resource"`
	Name     string   `json:"name"`
	Emails   []string `json:"emails,omitempty"`
	Phones   []string `json:"phones,omitempty"`
	Company  string   `json:"company,omitempty"`
	Title    string   `json:"title,omitempty"`
	Other    bool     `json:"other,omitempty"` // "other contacts" (auto-saved from e-mail)
}

// SetGoogle injects the Google integration.
func (a *Agent) SetGoogle(g GoogleAPI) {
	a.gMu.Lock()
	a.g = g
	a.gMu.Unlock()
}

// google returns the integration when service is usable.
func (a *Agent) google(service string) GoogleAPI {
	a.gMu.RLock()
	defer a.gMu.RUnlock()
	if a.g != nil && a.g.Can(service) {
		return a.g
	}
	return nil
}

// Addresses extracts lowercase e-mail addresses from header-style lists.
func Addresses(s string) []string {
	var out []string
	if list, err := mail.ParseAddressList(s); err == nil {
		for _, a := range list {
			out = append(out, strings.ToLower(a.Address))
		}
		return out
	}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '<' || r == '>' }) {
		if strings.Contains(f, "@") {
			out = append(out, strings.ToLower(strings.Trim(f, `"'`)))
		}
	}
	return out
}

// LinkPeople connects nodeID to the person nodes owning the given e-mails.
func (a *Agent) LinkPeople(ctx context.Context, nodeID int64, emails []string, relation string) error {
	seen := map[int64]bool{}
	for _, e := range emails {
		p, err := a.db.FindPersonByEmail(ctx, e)
		if err != nil || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		if err := a.db.AddEdge(ctx, nodeID, p.ID, relation, 1); err != nil {
			return err
		}
	}
	return nil
}
