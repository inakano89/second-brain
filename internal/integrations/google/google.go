// Package google integrates Google Calendar and Gmail through OAuth2.
package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
)

const tokenKey = "google.oauth_token"

// Scopes requested during consent.
var Scopes = []string{
	"https://www.googleapis.com/auth/calendar.events",
	"https://www.googleapis.com/auth/gmail.readonly",
}

// Endpoint is Google's OAuth2 endpoint (declared inline to avoid heavy deps).
var Endpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// Client talks to Google APIs on behalf of the owner.
type Client struct {
	cfg *config.Config
	db  *database.DB
	log *slog.Logger
	mu  sync.Mutex
}

// New creates a Google client.
func New(cfg *config.Config, db *database.DB, log *slog.Logger) *Client {
	return &Client{cfg: cfg, db: db, log: log.With("component", "google")}
}

func (c *Client) oauth() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.cfg.Get("GOOGLE_CLIENT_ID"),
		ClientSecret: c.cfg.Get("GOOGLE_CLIENT_SECRET"),
		Endpoint:     Endpoint,
		RedirectURL:  c.cfg.PublicURL() + "/google/callback",
		Scopes:       Scopes,
	}
}

// Configured reports whether OAuth credentials are set.
func (c *Client) Configured() bool {
	return c.cfg.Get("GOOGLE_CLIENT_ID") != "" && c.cfg.Get("GOOGLE_CLIENT_SECRET") != ""
}

// Connected reports whether a token is stored.
func (c *Client) Connected() bool {
	if !c.Configured() {
		return false
	}
	_, ok, _ := c.db.KVGet(context.Background(), tokenKey)
	return ok
}

// RedirectURL returns the callback URL to register in Google Cloud Console.
func (c *Client) RedirectURL() string { return c.oauth().RedirectURL }

// AuthURL builds the consent URL.
func (c *Client) AuthURL(state string) string {
	return c.oauth().AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

// Exchange stores the token obtained from the authorization code.
func (c *Client) Exchange(ctx context.Context, code string) error {
	tok, err := c.oauth().Exchange(ctx, code)
	if err != nil {
		return err
	}
	c.log.Info("Google conectado")
	return c.db.KVSetJSON(ctx, tokenKey, tok)
}

// Disconnect removes the stored token.
func (c *Client) Disconnect(ctx context.Context) error { return c.db.KVDelete(ctx, tokenKey) }

type savingSource struct {
	c    *Client
	src  oauth2.TokenSource
	last string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		_ = s.c.db.KVSetJSON(context.Background(), tokenKey, t)
	}
	return t, nil
}

func (c *Client) http(ctx context.Context) (*http.Client, error) {
	if !c.Configured() {
		return nil, errors.New("google: GOOGLE_CLIENT_ID/SECRET não configurados")
	}
	var tok oauth2.Token
	ok, err := c.db.KVGetJSON(ctx, tokenKey, &tok)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, agent.ErrNoCalendar
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	base := c.oauth().TokenSource(context.Background(), &tok)
	src := oauth2.ReuseTokenSource(&tok, &savingSource{c: c, src: base, last: tok.AccessToken})
	hc := oauth2.NewClient(context.Background(), src)
	hc.Timeout = 60 * time.Second
	return hc, nil
}

func (c *Client) do(ctx context.Context, method, u string, body any, out any) error {
	hc, err := c.http(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("google %s: HTTP %d: %s", u, resp.StatusCode, string(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- Calendar ----

type gTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type gEvent struct {
	ID          string `json:"id,omitempty"`
	Status      string `json:"status,omitempty"`
	Summary     string `json:"summary"`
	Description string `json:"description,omitempty"`
	Location    string `json:"location,omitempty"`
	HTMLLink    string `json:"htmlLink,omitempty"`
	Start       gTime  `json:"start"`
	End         gTime  `json:"end"`
}

func (c *Client) toEvent(e gEvent) agent.CalendarEvent {
	loc := c.cfg.Location()
	parse := func(t gTime) (time.Time, bool) {
		if t.DateTime != "" {
			v, _ := time.Parse(time.RFC3339, t.DateTime)
			return v, false
		}
		v, _ := time.ParseInLocation("2006-01-02", t.Date, loc)
		return v, true
	}
	s, allDay := parse(e.Start)
	en, _ := parse(e.End)
	return agent.CalendarEvent{ID: e.ID, Summary: e.Summary, Description: e.Description, Location: e.Location, Start: s, End: en, AllDay: allDay, Link: e.HTMLLink}
}

func (c *Client) calendarID() string { return url.PathEscape(c.cfg.Get("GOOGLE_CALENDAR_ID")) }

// ListEvents implements agent.CalendarAPI.
func (c *Client) ListEvents(ctx context.Context, from, to time.Time) ([]agent.CalendarEvent, error) {
	q := url.Values{}
	q.Set("timeMin", from.Format(time.RFC3339))
	q.Set("timeMax", to.Format(time.RFC3339))
	q.Set("singleEvents", "true")
	q.Set("orderBy", "startTime")
	q.Set("maxResults", "250")
	var resp struct {
		Items []gEvent `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "https://www.googleapis.com/calendar/v3/calendars/"+c.calendarID()+"/events?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]agent.CalendarEvent, 0, len(resp.Items))
	for _, e := range resp.Items {
		if e.Status == "cancelled" {
			continue
		}
		out = append(out, c.toEvent(e))
	}
	return out, nil
}

// CreateEvent implements agent.CalendarAPI.
func (c *Client) CreateEvent(ctx context.Context, ev agent.CalendarEvent) (*agent.CalendarEvent, error) {
	loc := c.cfg.Location()
	ge := gEvent{Summary: ev.Summary, Description: ev.Description, Location: ev.Location}
	if ev.AllDay {
		ge.Start = gTime{Date: ev.Start.In(loc).Format("2006-01-02")}
		ge.End = gTime{Date: ev.End.In(loc).Format("2006-01-02")}
	} else {
		ge.Start = gTime{DateTime: ev.Start.Format(time.RFC3339), TimeZone: loc.String()}
		ge.End = gTime{DateTime: ev.End.Format(time.RFC3339), TimeZone: loc.String()}
	}
	var created gEvent
	if err := c.do(ctx, http.MethodPost, "https://www.googleapis.com/calendar/v3/calendars/"+c.calendarID()+"/events", ge, &created); err != nil {
		return nil, err
	}
	e := c.toEvent(created)
	c.log.Info("evento criado no Google Calendar", "summary", e.Summary, "start", e.Start)
	return &e, nil
}

// ---- Gmail ----

// Email is a simplified Gmail message.
type Email struct {
	ID       string
	ThreadID string
	From     string
	Subject  string
	Date     time.Time
	Snippet  string
	Body     string
	Link     string
}

type gPart struct {
	MimeType string `json:"mimeType"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts   []gPart `json:"parts"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
}

// ListMessageIDs returns ids matching a Gmail search query.
func (c *Client) ListMessageIDs(ctx context.Context, query string, max int) ([]string, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("maxResults", fmt.Sprint(max))
	var resp struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := c.do(ctx, http.MethodGet, "https://gmail.googleapis.com/gmail/v1/users/me/messages?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	ids := make([]string, len(resp.Messages))
	for i, m := range resp.Messages {
		ids[i] = m.ID
	}
	return ids, nil
}

// GetMessage fetches and decodes a message.
func (c *Client) GetMessage(ctx context.Context, id string) (*Email, error) {
	var m struct {
		ID           string `json:"id"`
		ThreadID     string `json:"threadId"`
		Snippet      string `json:"snippet"`
		InternalDate string `json:"internalDate"`
		Payload      gPart  `json:"payload"`
	}
	if err := c.do(ctx, http.MethodGet, "https://gmail.googleapis.com/gmail/v1/users/me/messages/"+url.PathEscape(id)+"?format=full", nil, &m); err != nil {
		return nil, err
	}
	e := &Email{ID: m.ID, ThreadID: m.ThreadID, Snippet: m.Snippet, Link: "https://mail.google.com/mail/u/0/#all/" + m.ID}
	for _, h := range m.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			e.From = h.Value
		case "subject":
			e.Subject = h.Value
		}
	}
	var ms int64
	fmt.Sscan(m.InternalDate, &ms)
	e.Date = time.UnixMilli(ms)
	plain, htmlBody := walkParts(m.Payload)
	e.Body = plain
	if strings.TrimSpace(e.Body) == "" && htmlBody != "" {
		e.Body = extract.HTMLToText(htmlBody)
	}
	return e, nil
}

func decodeB64(s string) string {
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		b, _ = base64.RawURLEncoding.DecodeString(s)
	}
	return string(b)
}

func walkParts(p gPart) (plain, html string) {
	switch {
	case p.MimeType == "text/plain" && p.Body.Data != "":
		plain = decodeB64(p.Body.Data)
	case p.MimeType == "text/html" && p.Body.Data != "":
		html = decodeB64(p.Body.Data)
	}
	for _, sp := range p.Parts {
		pl, h := walkParts(sp)
		if plain == "" {
			plain = pl
		}
		if html == "" {
			html = h
		}
	}
	return
}
