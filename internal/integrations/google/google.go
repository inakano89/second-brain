// Package google integrates Google Calendar, Gmail, Drive, Contacts, Tasks and
// YouTube through OAuth2. Everything is read-only except calendar events and
// Gmail drafts.
package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/queue"
)

const (
	tokenKey  = "google.oauth_token"
	scopesKey = "google.scopes"
	scopeURL  = "https://www.googleapis.com/auth/"
)

// Service is a Google product the integration can use.
type Service struct {
	Key      string
	Label    string
	API      string   // API to enable in Google Cloud Console
	Scopes   []string // required
	Optional []string // requested, but the service works without them
}

// Services lists every supported product, in display order (GOOGLE_SERVICES keys).
var Services = []Service{
	{Key: agent.GoogleCalendar, Label: "Agenda: ler todas as agendas e criar eventos", API: "Google Calendar API",
		Scopes: []string{scopeURL + "calendar.events"}, Optional: []string{scopeURL + "calendar.readonly"}},
	{Key: agent.GoogleGmail, Label: "Gmail: leitura", API: "Gmail API", Scopes: []string{scopeURL + "gmail.readonly"}},
	{Key: agent.GoogleDrafts, Label: "Gmail: criar rascunhos (nunca envia)", API: "Gmail API", Scopes: []string{scopeURL + "gmail.compose"}},
	{Key: agent.GoogleDrive, Label: "Drive: leitura (e Takeout automático)", API: "Google Drive API", Scopes: []string{scopeURL + "drive.readonly"}},
	{Key: agent.GoogleContacts, Label: "Contatos: leitura", API: "People API",
		Scopes: []string{scopeURL + "contacts.readonly", scopeURL + "contacts.other.readonly"}},
	{Key: agent.GoogleTasks, Label: "Google Tasks: leitura", API: "Google Tasks API", Scopes: []string{scopeURL + "tasks.readonly"}},
	{Key: agent.GoogleYouTube, Label: "YouTube: curtidos, inscrições e playlists", API: "YouTube Data API v3", Scopes: []string{scopeURL + "youtube.readonly"}},
}

// legacyScopes were requested by versions that did not record granted scopes.
var legacyScopes = []string{scopeURL + "calendar.events", scopeURL + "gmail.readonly"}

const scopeCalendarRead = scopeURL + "calendar.readonly"

// Endpoint is Google's OAuth2 endpoint (declared inline to avoid heavy deps).
var Endpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// ErrNotConnected means no OAuth token is stored.
var ErrNotConnected = errors.New("google não conectado: Configurações → Google → Conectar")

// Client talks to Google APIs on behalf of the owner.
type Client struct {
	cfg *config.Config
	db  *database.DB
	log *slog.Logger

	mu     sync.Mutex // guards src/srcKey
	src    oauth2.TokenSource
	srcKey string

	scopeMu    sync.Mutex
	scopes     map[string]bool
	scopesAt   time.Time
	scopesSure bool

	warm atomic.Bool // People API search cache warmed up

	base *http.Client // underlying HTTP client (tests)
}

var tokenInfoURL = "https://oauth2.googleapis.com/tokeninfo"

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
		Scopes:       c.RequestedScopes(),
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

// Enabled reports whether service is listed in GOOGLE_SERVICES.
func (c *Client) Enabled(service string) bool {
	for _, s := range c.cfg.GetList("GOOGLE_SERVICES") {
		if strings.EqualFold(s, service) {
			return true
		}
	}
	return false
}

func lookupService(key string) (Service, bool) {
	for _, s := range Services {
		if s.Key == key {
			return s, true
		}
	}
	return Service{}, false
}

// RequestedScopes returns the scopes of every enabled service.
func (c *Client) RequestedScopes() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range Services {
		if !c.Enabled(s.Key) {
			continue
		}
		for _, sc := range append(append([]string{}, s.Scopes...), s.Optional...) {
			if !seen[sc] {
				seen[sc] = true
				out = append(out, sc)
			}
		}
	}
	return out
}

// Can reports whether service is enabled, connected and authorised (agent.GoogleAPI).
func (c *Client) Can(service string) bool {
	s, ok := lookupService(service)
	if !ok || !c.Enabled(service) || !c.Connected() {
		return false
	}
	g := c.granted()
	for _, sc := range s.Scopes {
		if !g[sc] {
			return false
		}
	}
	return true
}

func (c *Client) hasScope(scope string) bool { return c.Connected() && c.granted()[scope] }

// ServiceStatus describes one service for the settings page.
type ServiceStatus struct {
	Key     string
	Label   string
	API     string
	Enabled bool
	Granted bool // every required scope granted
	Partial bool // required scopes granted, optional ones missing
}

// Status reports every service's state.
func (c *Client) Status() []ServiceStatus {
	connected := c.Connected()
	var g map[string]bool
	if connected {
		g = c.granted()
	}
	out := make([]ServiceStatus, 0, len(Services))
	for _, s := range Services {
		st := ServiceStatus{Key: s.Key, Label: s.Label, API: s.API, Enabled: c.Enabled(s.Key), Granted: connected}
		for _, sc := range s.Scopes {
			st.Granted = st.Granted && g[sc]
		}
		for _, sc := range s.Optional {
			if st.Granted && !g[sc] {
				st.Partial = true
			}
		}
		out = append(out, st)
	}
	return out
}

// NeedsReconnect reports whether an enabled service lacks a scope the user must grant.
func (c *Client) NeedsReconnect() bool {
	if !c.Connected() {
		return false
	}
	for _, st := range c.Status() {
		if st.Enabled && (!st.Granted || st.Partial) {
			return true
		}
	}
	return false
}

func scopeSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(s) {
		m[f] = true
	}
	return m
}

// granted returns the scopes carried by the stored token (cached).
func (c *Client) granted() map[string]bool {
	c.scopeMu.Lock()
	defer c.scopeMu.Unlock()
	if c.scopes != nil && (c.scopesSure || time.Since(c.scopesAt) < 10*time.Minute) {
		return c.scopes
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if v, ok, _ := c.db.KVGet(ctx, scopesKey); ok {
		c.scopes, c.scopesSure = scopeSet(v), true
		return c.scopes
	}
	// Token stored before scope tracking: ask Google what it carries.
	v, err := c.tokenScopes(ctx)
	if err == nil {
		_ = c.db.KVSet(ctx, scopesKey, v)
		c.scopes, c.scopesSure = scopeSet(v), true
		return c.scopes
	}
	c.log.Debug("tokeninfo indisponível; assumindo escopos antigos", "err", err)
	c.scopes, c.scopesAt, c.scopesSure = scopeSet(strings.Join(legacyScopes, " ")), time.Now(), false
	return c.scopes
}

func (c *Client) resetScopes() {
	c.scopeMu.Lock()
	c.scopes, c.scopesSure = nil, false
	c.scopeMu.Unlock()
}

func (c *Client) tokenScopes(ctx context.Context) (string, error) {
	src, err := c.source(ctx)
	if err != nil {
		return "", err
	}
	tok, err := src.Token()
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tokenInfoURL+"?access_token="+url.QueryEscape(tok.AccessToken), nil)
	resp, err := c.httpBase().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var info struct {
		Scope string `json:"scope"`
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tokeninfo: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.Scope, nil
}

// RedirectURL returns the callback URL to register in Google Cloud Console.
func (c *Client) RedirectURL() string { return c.cfg.PublicURL() + "/google/callback" }

// AuthURL builds the consent URL.
func (c *Client) AuthURL(state string) string {
	return c.oauth().AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

// Exchange stores the token (and its granted scopes) obtained from the authorization code.
func (c *Client) Exchange(ctx context.Context, code string) error {
	tok, err := c.oauth().Exchange(ctx, code)
	if err != nil {
		return err
	}
	if err := c.db.KVSetJSON(ctx, tokenKey, tok); err != nil {
		return err
	}
	scope, _ := tok.Extra("scope").(string)
	if scope != "" {
		err = c.db.KVSet(ctx, scopesKey, scope)
	} else {
		err = c.db.KVDelete(ctx, scopesKey)
	}
	c.invalidate()
	c.log.Info("Google conectado", "scopes", len(strings.Fields(scope)))
	return err
}

// Disconnect revokes and removes the stored token.
func (c *Client) Disconnect(ctx context.Context) error {
	var tok oauth2.Token
	if ok, _ := c.db.KVGetJSON(ctx, tokenKey, &tok); ok {
		t := tok.RefreshToken
		if t == "" {
			t = tok.AccessToken
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodPost, "https://oauth2.googleapis.com/revoke", strings.NewReader(url.Values{"token": {t}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if resp, err := c.httpBase().Do(req); err == nil {
			resp.Body.Close()
		}
		cancel()
	}
	err := errors.Join(c.db.KVDelete(ctx, tokenKey), c.db.KVDelete(ctx, scopesKey))
	c.invalidate()
	return err
}

func (c *Client) invalidate() {
	c.mu.Lock()
	c.src = nil
	c.mu.Unlock()
	c.resetScopes()
	c.warm.Store(false)
}

type savingSource struct {
	c    *Client
	src  oauth2.TokenSource
	last string
	mu   sync.Mutex
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		_ = s.c.db.KVSetJSON(context.Background(), tokenKey, t)
	}
	return t, nil
}

// source returns a shared, auto-refreshing token source.
func (c *Client) source(ctx context.Context) (oauth2.TokenSource, error) {
	if !c.Configured() {
		return nil, errors.New("google: GOOGLE_CLIENT_ID/SECRET não configurados")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := c.cfg.Get("GOOGLE_CLIENT_ID") + "\x00" + c.cfg.Get("GOOGLE_CLIENT_SECRET")
	if c.src != nil && c.srcKey == key {
		return c.src, nil
	}
	var tok oauth2.Token
	ok, err := c.db.KVGetJSON(ctx, tokenKey, &tok)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotConnected
	}
	base := c.oauth().TokenSource(c.baseCtx(), &tok)
	c.src = oauth2.ReuseTokenSource(&tok, &savingSource{c: c, src: base, last: tok.AccessToken})
	c.srcKey = key
	return c.src, nil
}

func (c *Client) httpBase() *http.Client {
	if c.base != nil {
		return c.base
	}
	return http.DefaultClient
}

// baseCtx carries the underlying HTTP client for oauth2.
func (c *Client) baseCtx() context.Context {
	if c.base != nil {
		return context.WithValue(context.Background(), oauth2.HTTPClient, c.base)
	}
	return context.Background()
}

// APIError is a non-2xx answer from a Google API.
type APIError struct {
	Status  int
	API     string
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("google %s: HTTP %d: %s", e.API, e.Status, e.Message)
}

func isStatus(err error, codes ...int) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.Status == c {
			return true
		}
	}
	return false
}

func apiName(u string) string {
	switch {
	case strings.Contains(u, "gmail.googleapis.com"):
		return "Gmail API"
	case strings.Contains(u, "/calendar/"):
		return "Google Calendar API"
	case strings.Contains(u, "/drive/"):
		return "Google Drive API"
	case strings.Contains(u, "people.googleapis.com"):
		return "People API"
	case strings.Contains(u, "tasks.googleapis.com"):
		return "Google Tasks API"
	case strings.Contains(u, "/youtube/"):
		return "YouTube Data API v3"
	}
	return "API"
}

// classify turns an HTTP error body into an error, marking definitive ones as permanent.
func classify(u string, status int, body []byte) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	ae := &APIError{Status: status, API: apiName(u), Reason: e.Error.Status, Message: e.Error.Message}
	if len(e.Error.Errors) > 0 && e.Error.Errors[0].Reason != "" {
		ae.Reason = e.Error.Errors[0].Reason
	}
	if ae.Message == "" {
		ae.Message = strings.TrimSpace(string(body))
		if len(ae.Message) > 300 {
			ae.Message = ae.Message[:300]
		}
	}
	raw := string(body)
	switch {
	case status == http.StatusForbidden && (strings.Contains(raw, "accessNotConfigured") || strings.Contains(raw, "SERVICE_DISABLED")):
		ae.Message = "ative a " + ae.API + " no Google Cloud Console (APIs e serviços → Biblioteca)"
		return queue.Permanent(ae)
	case status == http.StatusForbidden && (strings.Contains(raw, "insufficientPermissions") || strings.Contains(raw, "ACCESS_TOKEN_SCOPE_INSUFFICIENT")):
		ae.Message = "permissão não concedida — reconecte o Google e marque todas as caixas"
		return queue.Permanent(ae)
	case status == http.StatusUnauthorized:
		ae.Message = "autorização inválida — reconecte o Google"
		return queue.Permanent(ae)
	case status == http.StatusTooManyRequests || status >= 500:
		return ae
	case status == http.StatusForbidden && (strings.Contains(raw, "ateLimitExceeded") || strings.Contains(raw, "quotaExceeded")):
		return ae
	}
	return queue.Permanent(ae)
}

// open performs an authorised request; the caller closes the body of a 2xx response.
func (c *Client) open(ctx context.Context, method, u string, body io.Reader, contentType string) (*http.Response, error) {
	src, err := c.source(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := oauth2.NewClient(c.baseCtx(), src).Do(req)
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && (re.ErrorCode == "invalid_grant" || re.ErrorCode == "unauthorized_client" || re.ErrorCode == "invalid_client") {
			return nil, queue.Permanentf("google: autorização expirada ou revogada (%s) — reconecte em Configurações → Google", re.ErrorCode)
		}
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, classify(u, resp.StatusCode, b)
	}
	return resp, nil
}

// do sends an optional JSON body and decodes a JSON answer into out.
func (c *Client) do(ctx context.Context, method, u string, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var rd io.Reader
	ct := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd, ct = strings.NewReader(string(b)), "application/json"
	}
	resp, err := c.open(ctx, method, u, rd, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// readAll GETs u and returns at most limit bytes (error when larger).
func (c *Client) readAll(ctx context.Context, u string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	resp, err := c.open(ctx, http.MethodGet, u, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, queue.Permanentf("arquivo maior que %d MB", limit>>20)
	}
	return b, nil
}
