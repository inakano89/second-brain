package google

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/extract"
)

// Email is a simplified Gmail message.
type Email = agent.EmailInfo

type gHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gPart struct {
	MimeType string `json:"mimeType"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts   []gPart   `json:"parts"`
	Headers []gHeader `json:"headers"`
}

type gMessage struct {
	ID           string `json:"id"`
	ThreadID     string `json:"threadId"`
	Snippet      string `json:"snippet"`
	InternalDate string `json:"internalDate"`
	Payload      gPart  `json:"payload"`
}

func header(hs []gHeader, name string) string {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func (m *gMessage) email() *Email {
	e := &Email{ID: m.ID, ThreadID: m.ThreadID, Snippet: m.Snippet, Link: "https://mail.google.com/mail/u/0/#all/" + m.ID,
		From: header(m.Payload.Headers, "From"), To: header(m.Payload.Headers, "To"), Subject: header(m.Payload.Headers, "Subject")}
	var ms int64
	fmt.Sscan(m.InternalDate, &ms)
	e.Date = time.UnixMilli(ms)
	return e
}

const gmailAPI = "https://gmail.googleapis.com/gmail/v1/users/me/"

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
	if err := c.do(ctx, http.MethodGet, gmailAPI+"messages?"+q.Encode(), nil, &resp); err != nil {
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
	var m gMessage
	if err := c.do(ctx, http.MethodGet, gmailAPI+"messages/"+url.PathEscape(id)+"?format=full", nil, &m); err != nil {
		return nil, err
	}
	e := m.email()
	plain, htmlBody := walkParts(m.Payload)
	e.Body = plain
	if strings.TrimSpace(e.Body) == "" && htmlBody != "" {
		e.Body = extract.HTMLToText(htmlBody)
	}
	return e, nil
}

func (c *Client) messageMeta(ctx context.Context, id string, headers ...string) (*gMessage, error) {
	q := url.Values{"format": {"metadata"}}
	for _, h := range headers {
		q.Add("metadataHeaders", h)
	}
	var m gMessage
	if err := c.do(ctx, http.MethodGet, gmailAPI+"messages/"+url.PathEscape(id)+"?"+q.Encode(), nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// SearchEmail implements agent.GoogleAPI (metadata fetched concurrently).
func (c *Client) SearchEmail(ctx context.Context, query string, max int) ([]agent.EmailInfo, error) {
	ids, err := c.ListMessageIDs(ctx, query, max)
	if err != nil {
		return nil, err
	}
	out := make([]agent.EmailInfo, len(ids))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(5)
	for i, id := range ids {
		g.Go(func() error {
			m, err := c.messageMeta(gctx, id, "From", "To", "Subject")
			if err != nil {
				return err
			}
			out[i] = *m.email()
			return nil
		})
	}
	return out, g.Wait()
}

// ReadEmail implements agent.GoogleAPI.
func (c *Client) ReadEmail(ctx context.Context, id string) (*agent.EmailInfo, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("id do e-mail vazio")
	}
	return c.GetMessage(ctx, id)
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

// ---- drafts ----

var headerSafe = strings.NewReplacer("\r", " ", "\n", " ")

func formatAddrs(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	list, err := mail.ParseAddressList(s)
	if err != nil {
		return "", fmt.Errorf("endereço de e-mail inválido %q: %w", s, err)
	}
	parts := make([]string, len(list))
	for i, a := range list {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", "), nil
}

// buildRaw renders an RFC 5322 text/plain message (UTF-8, base64 body).
func buildRaw(to, cc, subject, body, inReplyTo, references string) string {
	var b strings.Builder
	w := func(k, v string) {
		if v != "" {
			b.WriteString(k + ": " + headerSafe.Replace(v) + "\r\n")
		}
	}
	w("To", to)
	w("Cc", cc)
	w("Subject", mime.QEncoding.Encode("utf-8", headerSafe.Replace(subject)))
	w("In-Reply-To", inReplyTo)
	w("References", references)
	w("MIME-Version", "1.0")
	w("Content-Type", `text/plain; charset="UTF-8"`)
	w("Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(body, "\r\n", "\n")))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String()
}

func replySubject(s string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "re:") {
		return s
	}
	return "Re: " + s
}

// CreateDraft implements agent.GoogleAPI: the message is saved as a draft, never sent.
func (c *Client) CreateDraft(ctx context.Context, d agent.EmailDraft) (*agent.DraftResult, error) {
	var inReplyTo, refs, threadID string
	if d.ReplyTo != "" {
		m, err := c.messageMeta(ctx, d.ReplyTo, "Message-ID", "References", "Subject", "From", "Reply-To")
		if err != nil {
			return nil, err
		}
		h := m.Payload.Headers
		threadID = m.ThreadID
		inReplyTo = header(h, "Message-ID")
		refs = strings.TrimSpace(header(h, "References") + " " + inReplyTo)
		if d.Subject == "" {
			d.Subject = replySubject(header(h, "Subject"))
		}
		if d.To == "" {
			d.To = header(h, "Reply-To")
			if d.To == "" {
				d.To = header(h, "From")
			}
		}
	}
	to, err := formatAddrs(d.To)
	if err != nil {
		return nil, err
	}
	cc, err := formatAddrs(d.Cc)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.Body) == "" {
		return nil, errors.New("o corpo do e-mail está vazio")
	}
	msg := map[string]string{"raw": base64.URLEncoding.EncodeToString([]byte(buildRaw(to, cc, d.Subject, d.Body, inReplyTo, refs)))}
	if threadID != "" {
		msg["threadId"] = threadID
	}
	var resp struct {
		ID      string `json:"id"`
		Message struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		} `json:"message"`
	}
	if err := c.do(ctx, http.MethodPost, gmailAPI+"drafts", map[string]any{"message": msg}, &resp); err != nil {
		return nil, err
	}
	c.log.Info("rascunho criado no Gmail", "subject", d.Subject)
	return &agent.DraftResult{ID: resp.ID, ThreadID: resp.Message.ThreadID, To: to, Subject: d.Subject,
		Link: "https://mail.google.com/mail/u/0/#drafts?compose=" + resp.Message.ID}, nil
}
