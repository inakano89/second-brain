// Package telegram implements the Telegram Bot API channel: whitelisted
// conversation, note/task capture, voice transcription and image OCR.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client is a minimal Telegram Bot API client.
type Client struct {
	token string
	base  string
	http  *http.Client
}

// NewClient creates a client for token.
func NewClient(token string) *Client {
	return &Client{token: token, base: "https://api.telegram.org", http: &http.Client{Timeout: 90 * time.Second}}
}

// APIError is a Telegram error response.
type APIError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Description) }

type apiResp struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/bot%s/%s", c.base, c.token, method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp.Body, out)
}

func decode(r io.Reader, out any) error {
	var ar apiResp
	if err := json.NewDecoder(r).Decode(&ar); err != nil {
		return err
	}
	if !ar.OK {
		e := &APIError{Code: ar.ErrorCode, Description: ar.Description}
		if ar.Parameters != nil {
			e.RetryAfter = ar.Parameters.RetryAfter
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(ar.Result, out)
	}
	return nil
}

// Types (subset).
type (
	User struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	}
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	}
	FileRef struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
		MimeType string `json:"mime_type"`
		FileName string `json:"file_name"`
		Duration int    `json:"duration"`
	}
	PhotoSize struct {
		FileID   string `json:"file_id"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		FileSize int64  `json:"file_size"`
	}
	Message struct {
		MessageID int64       `json:"message_id"`
		From      *User       `json:"from"`
		Chat      Chat        `json:"chat"`
		Date      int64       `json:"date"`
		Text      string      `json:"text"`
		Caption   string      `json:"caption"`
		Voice     *FileRef    `json:"voice"`
		Audio     *FileRef    `json:"audio"`
		VideoNote *FileRef    `json:"video_note"`
		Document  *FileRef    `json:"document"`
		Photo     []PhotoSize `json:"photo"`
	}
	Update struct {
		UpdateID int64    `json:"update_id"`
		Message  *Message `json:"message"`
	}
	File struct {
		FileID   string `json:"file_id"`
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
)

// GetMe validates the token.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var u User
	return &u, c.call(ctx, "getMe", map[string]any{}, &u)
}

// DeleteWebhook ensures long polling works.
func (c *Client) DeleteWebhook(ctx context.Context) error {
	return c.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

// GetUpdates long-polls for updates.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": timeout, "allowed_updates": []string{"message"}}, &ups)
	return ups, err
}

// SendMessage sends text (Markdown with plain-text fallback) and returns the message id.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (int64, error) {
	var last int64
	for _, part := range Split(text, 4000) {
		params := map[string]any{"chat_id": chatID, "text": part, "parse_mode": "Markdown", "link_preview_options": map[string]any{"is_disabled": true}}
		if replyTo != 0 {
			params["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
		}
		var m Message
		err := c.call(ctx, "sendMessage", params, &m)
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == 400 {
			delete(params, "parse_mode")
			err = c.call(ctx, "sendMessage", params, &m)
		}
		if err != nil {
			return last, err
		}
		last = m.MessageID
		replyTo = 0
	}
	return last, nil
}

// EditMessage replaces a message's text (plain when markdown=false).
func (c *Client) EditMessage(ctx context.Context, chatID, msgID int64, text string, markdown bool) error {
	params := map[string]any{"chat_id": chatID, "message_id": msgID, "text": text, "link_preview_options": map[string]any{"is_disabled": true}}
	if markdown {
		params["parse_mode"] = "Markdown"
	}
	err := c.call(ctx, "editMessageText", params, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == 400 {
		if strings.Contains(ae.Description, "not modified") {
			return nil
		}
		if markdown {
			delete(params, "parse_mode")
			return c.call(ctx, "editMessageText", params, nil)
		}
	}
	return err
}

// SendChatAction shows "typing…" etc.
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) {
	_ = c.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action}, nil)
}

// DownloadFile fetches a file by id into dest (max 20 MB per Bot API).
func (c *Client) DownloadFile(ctx context.Context, fileID, dest string) error {
	var f File
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/file/bot%s/%s", c.base, c.token, f.FilePath), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("telegram download: HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(resp.Body, 25<<20)); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	return out.Close()
}

// SendDocument uploads a file (streamed multipart via goroutine + pipe).
func (c *Client) SendDocument(ctx context.Context, chatID int64, filename string, r io.Reader, caption string) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if err := mw.WriteField("chat_id", fmt.Sprint(chatID)); err != nil {
				return err
			}
			if caption != "" {
				if err := mw.WriteField("caption", caption); err != nil {
					return err
				}
			}
			fw, err := mw.CreateFormFile("document", filename)
			if err != nil {
				return err
			}
			if _, err := io.Copy(fw, r); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/bot%s/sendDocument", c.base, c.token), pr)
	if err != nil {
		pr.Close()
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp.Body, nil)
}

// Split breaks text into chunks of at most n bytes on line boundaries.
func Split(text string, n int) []string {
	if len(text) <= n {
		return []string{text}
	}
	var out []string
	for len(text) > n {
		cut := strings.LastIndex(text[:n], "\n")
		if cut < n/2 {
			cut = n
			for cut > 0 && (text[cut]&0xC0) == 0x80 {
				cut--
			}
		}
		out = append(out, text[:cut])
		text = strings.TrimLeft(text[cut:], "\n")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}
