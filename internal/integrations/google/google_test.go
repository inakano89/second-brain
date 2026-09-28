package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/queue"
)

// rewrite sends every request to the fake server, keeping the original host in a header.
type rewrite struct{ target *url.URL }

func (rw rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("X-Host", req.URL.Host)
	r.URL.Scheme, r.URL.Host, r.Host = rw.target.Scheme, rw.target.Host, rw.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

type fakeGoogle struct {
	mu       sync.Mutex
	drafts   []map[string]any
	queries  map[string]string
	takeouts []map[string]any
}

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("X-Host") + r.URL.Path
	f.mu.Lock()
	f.queries[key] = r.URL.RawQuery
	f.mu.Unlock()
	out := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	type m = map[string]any
	type a = []any
	switch key {
	case "oauth2.googleapis.com/tokeninfo":
		out(m{"scope": scopeURL + "calendar.events " + scopeURL + "gmail.readonly"})
	case "www.googleapis.com/calendar/v3/users/me/calendarList":
		out(m{"items": a{
			m{"id": "eu@x.com", "summary": "eu@x.com", "primary": true, "selected": true},
			m{"id": "familia@group", "summary": "Família", "selected": true},
			m{"id": "oculta@group", "summary": "Oculta", "hidden": true},
		}})
	case "www.googleapis.com/calendar/v3/calendars/eu@x.com/events":
		out(m{"items": a{m{"id": "E1", "summary": "Reunião orçamento", "start": m{"dateTime": "2026-09-29T10:00:00-03:00"}, "end": m{"dateTime": "2026-09-29T11:00:00-03:00"},
			"attendees": a{m{"email": "eu@x.com", "self": true}, m{"email": "Ana@X.com", "displayName": "Ana Lima", "responseStatus": "accepted"}, m{"email": "sala@resource", "resource": true}}}}})
	case "www.googleapis.com/calendar/v3/calendars/familia@group/events":
		out(m{"items": a{
			m{"id": "E1", "summary": "Reunião orçamento", "start": m{"dateTime": "2026-09-29T10:00:00-03:00"}, "end": m{"dateTime": "2026-09-29T11:00:00-03:00"}},
			m{"id": "E2", "summary": "Aniversário da vó", "start": m{"date": "2026-09-30"}, "end": m{"date": "2026-10-01"}},
			m{"id": "E3", "status": "cancelled", "summary": "Cancelado"},
		}})
	case "people.googleapis.com/v1/people/me/connections":
		out(m{"connections": a{m{"resourceName": "people/c1", "names": a{m{"displayName": "Ana Lima"}},
			"emailAddresses": a{m{"value": "ana@x.com"}}, "phoneNumbers": a{m{"value": "(11) 99999-0000", "canonicalForm": "+5511999990000", "formattedType": "Celular"}},
			"organizations": a{m{"name": "Acme", "title": "CFO"}}, "birthdays": a{m{"date": m{"month": 3, "day": 12}}}, "biographies": a{m{"value": "Conheci na faculdade."}}}}})
	case "people.googleapis.com/v1/people:searchContacts":
		out(m{"results": a{m{"person": m{"resourceName": "people/c1", "names": a{m{"displayName": "Ana Lima"}}, "emailAddresses": a{m{"value": "ana@x.com"}}}}}})
	case "people.googleapis.com/v1/otherContacts:search":
		out(m{"results": a{m{"person": m{"resourceName": "otherContacts/o1", "emailAddresses": a{m{"value": "ana@x.com"}}}}, m{"person": m{"resourceName": "otherContacts/o2", "emailAddresses": a{m{"value": "bruno@y.com"}}}}}})
	case "tasks.googleapis.com/tasks/v1/users/@me/lists":
		out(m{"items": a{m{"id": "L1", "title": "Pessoal"}}})
	case "tasks.googleapis.com/tasks/v1/lists/L1/tasks":
		out(m{"items": a{
			m{"id": "T1", "title": "Declarar IR", "status": "needsAction", "due": "2026-10-01T00:00:00.000Z", "updated": "2026-09-01T00:00:00.000Z"},
			m{"id": "T2", "title": "Juntar recibos", "status": "completed", "parent": "T1", "updated": "2026-09-01T00:00:00.000Z"},
			m{"id": "T3", "title": "Apagada", "deleted": true, "updated": "2026-09-01T00:00:00.000Z"},
		}})
	case "www.googleapis.com/drive/v3/files":
		if q := r.URL.Query().Get("q"); strings.Contains(q, "takeout-") {
			since := ""
			if _, after, ok := strings.Cut(q, "createdTime > '"); ok {
				since, _, _ = strings.Cut(after, "'")
			}
			var files a
			for _, tk := range f.takeouts {
				if since == "" || tk["createdTime"].(string) > since {
					files = append(files, tk)
				}
			}
			out(m{"files": files})
			return
		}
		out(m{"files": a{
			m{"id": "doc1", "name": "Plano 2027", "mimeType": mimeGDoc, "modifiedTime": "2026-09-01T10:00:00.000Z", "createdTime": "2026-08-01T10:00:00.000Z", "webViewLink": "https://docs/doc1", "owners": a{m{"displayName": "Eu"}}},
			m{"id": "txt1", "name": "notas.txt", "mimeType": "text/plain", "modifiedTime": "2026-09-02T10:00:00.000Z", "size": "12"},
			m{"id": "big", "name": "enorme.pdf", "mimeType": "application/pdf", "modifiedTime": "2026-09-03T10:00:00.000Z", "size": "31457280"},
		}})
	case "www.googleapis.com/drive/v3/files/doc1/export":
		if r.URL.Query().Get("mimeType") == "text/markdown" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":400,"message":"export format not supported"}}`)
			return
		}
		io.WriteString(w, "Plano\nMeta: viajar para o Japão")
	case "www.googleapis.com/drive/v3/files/txt1":
		io.WriteString(w, "lembrar do dentista")
	case "www.googleapis.com/youtube/v3/videos":
		out(m{"items": a{m{"id": "v1", "snippet": m{"title": "Como fazer pão", "channelTitle": "Cozinha", "description": "Receita", "publishedAt": "2025-01-01T00:00:00Z"}}}})
	case "www.googleapis.com/youtube/v3/subscriptions":
		out(m{"items": a{m{"snippet": m{"title": "Canal Dev", "description": "Programação", "resourceId": m{"channelId": "UC1"}}}}})
	case "www.googleapis.com/youtube/v3/playlists":
		out(m{"items": a{m{"id": "PL1", "snippet": m{"title": "Estudos"}, "contentDetails": m{"itemCount": 1}}}})
	case "www.googleapis.com/youtube/v3/playlistItems":
		out(m{"items": a{m{"snippet": m{"title": "Aula 1", "videoOwnerChannelTitle": "Canal Dev"}}}})
	case "gmail.googleapis.com/gmail/v1/users/me/messages":
		out(m{"messages": a{m{"id": "m1"}}})
	case "gmail.googleapis.com/gmail/v1/users/me/messages/m1":
		out(m{"id": "m1", "threadId": "t1", "snippet": "segue", "internalDate": "1790000000000", "payload": m{"headers": a{
			m{"name": "From", "value": "Ana Lima <ana@x.com>"}, m{"name": "To", "value": "eu@x.com"}, m{"name": "Subject", "value": "Orçamento"},
			m{"name": "Message-Id", "value": "<abc@mail>"}}}})
	case "gmail.googleapis.com/gmail/v1/users/me/drafts":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.drafts = append(f.drafts, body)
		f.mu.Unlock()
		out(m{"id": "r1", "message": m{"id": "msg9", "threadId": "t1"}})
	case "www.googleapis.com/drive/v3/files/new1", "www.googleapis.com/drive/v3/files/new2", "www.googleapis.com/drive/v3/files/next1":
		io.WriteString(w, "zip-"+path.Base(r.URL.Path))
	case "www.googleapis.com/drive/v3/files/off":
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"message":"Drive API has not been used","errors":[{"reason":"accessNotConfigured"}],"status":"PERMISSION_DENIED"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":404,"message":"unknown `+key+`"}}`)
	}
}

type testEnv struct {
	c    *Client
	s    *Syncer
	db   *database.DB
	cfg  *config.Config
	fake *fakeGoogle
}

func setup(t *testing.T, scopes string, extra map[string]string) *testEnv {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]string{"GOOGLE_CLIENT_ID": "id", "GOOGLE_CLIENT_SECRET": "secret", "TIMEZONE": "America/Sao_Paulo", "DATA_DIR": dir, "GOOGLE_CALENDAR_PAST_DAYS": "0"}
	for k, v := range extra {
		vals[k] = v
	}
	if err := cfg.Update(vals); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fake := &fakeGoogle{queries: map[string]string{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(cfg, db, log)
	c.base = &http.Client{Transport: rewrite{u}}
	ctx := context.Background()
	if err := db.KVSetJSON(ctx, tokenKey, oauth2.Token{AccessToken: "at", RefreshToken: "rt", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if scopes != "" {
		db.KVSet(ctx, scopesKey, scopes)
	}
	ag := agent.New(cfg, db, llm.NewManager(cfg, nil), log)
	ag.SetGoogle(c)
	return &testEnv{c: c, s: NewSyncer(c, ag), db: db, cfg: cfg, fake: fake}
}

func allScopes() string {
	var all []string
	for _, s := range Services {
		all = append(all, s.Scopes...)
		all = append(all, s.Optional...)
	}
	return strings.Join(all, " ")
}

func TestLegacyTokenScopesAndStatus(t *testing.T) {
	e := setup(t, "", nil)
	if !e.c.Can(agent.GoogleCalendar) || !e.c.Can(agent.GoogleGmail) || e.c.Can(agent.GoogleDrive) || e.c.Can(agent.GoogleDrafts) {
		t.Fatal("legacy token should allow only calendar and gmail")
	}
	if v, ok, _ := e.db.KVGet(context.Background(), scopesKey); !ok || !strings.Contains(v, "gmail.readonly") {
		t.Fatalf("tokeninfo scopes not cached: %q", v)
	}
	var cal ServiceStatus
	for _, st := range e.c.Status() {
		if st.Key == agent.GoogleCalendar {
			cal = st
		}
	}
	if !cal.Granted || !cal.Partial || !e.c.NeedsReconnect() {
		t.Fatalf("calendar status = %+v, reconnect=%v", cal, e.c.NeedsReconnect())
	}
	if err := e.cfg.Update(map[string]string{"GOOGLE_SERVICES": "gmail"}); err != nil {
		t.Fatal(err)
	}
	if e.c.Can(agent.GoogleCalendar) || strings.Join(e.c.RequestedScopes(), " ") != scopeURL+"gmail.readonly" || e.c.NeedsReconnect() {
		t.Fatalf("service toggle ignored: %v", e.c.RequestedScopes())
	}
}

func TestSyncEverything(t *testing.T) {
	e := setup(t, allScopes(), nil)
	ctx := context.Background()
	db := e.db

	// An auto-created person is adopted by the matching contact.
	auto := &database.Node{Type: database.TypePerson, Title: "Ana Lima", Source: "agent", Meta: map[string]any{"auto": true}}
	if err := db.CreateNode(ctx, auto); err != nil {
		t.Fatal(err)
	}
	if n, err := e.s.SyncContacts(ctx); err != nil || n != 1 {
		t.Fatalf("contacts = %d, %v", n, err)
	}
	ana, err := db.FindPersonByEmail(ctx, "ana@x.com")
	if err != nil || ana.ID != auto.ID || ana.Source != "contacts" || !strings.Contains(ana.Content, "**Trabalho:** Acme — CFO") || !strings.Contains(ana.Content, "12/03") || ana.Meta["birthday"] != "03-12" {
		t.Fatalf("contact = %+v, %v", ana, err)
	}
	if n, _ := e.s.SyncContacts(ctx); n != 0 {
		t.Fatal("unchanged contact rewritten")
	}

	if n, err := e.s.SyncCalendar(ctx); err != nil || n != 2 {
		t.Fatalf("calendar = %d, %v", n, err)
	}
	ev, err := db.GetNodeBySource(ctx, "calendar", "E1")
	if err != nil || !strings.Contains(ev.Content, "Ana Lima <ana@x.com>") || strings.Contains(ev.Content, "sala@resource") || ev.Meta["calendar"] != "Principal" {
		t.Fatalf("event = %+v, %v", ev, err)
	}
	links, _ := db.Neighbors(ctx, ev.ID)
	linked := false
	for _, l := range links {
		linked = linked || (l.Node.ID == ana.ID && l.Relation == "attendee")
	}
	if !linked {
		t.Fatal("attendee not linked to contact")
	}
	if _, err := db.GetNodeBySource(ctx, "calendar", "E3"); err == nil {
		t.Fatal("cancelled event imported")
	}
	// A contact created after the event is linked back to it.
	if ids, err := db.NodesWithMetaValue(ctx, database.TypeEvent, "attendees", "ANA@x.com"); err != nil || len(ids) != 1 || ids[0] != ev.ID {
		t.Fatalf("event attendees lookup = %v, %v", ids, err)
	}

	if n, err := e.s.SyncTasks(ctx); err != nil || n != 2 {
		t.Fatalf("tasks = %d, %v", n, err)
	}
	t1, _ := db.GetNodeBySource(ctx, "gtasks", "T1")
	t2, _ := db.GetNodeBySource(ctx, "gtasks", "T2")
	if t1 == nil || t2 == nil || t1.Status != database.StatusOpen || t2.Status != database.StatusDone || t1.DueAt == nil ||
		t1.DueAt.In(e.cfg.Location()).Format("2006-01-02 15:04") != "2026-10-01 00:00" {
		t.Fatalf("tasks = %+v / %+v", t1, t2)
	}
	if !strings.Contains(e.fake.queries["tasks.googleapis.com/tasks/v1/lists/L1/tasks"], "showDeleted=true") {
		t.Fatal("deleted tasks not requested")
	}
	db.SetStatus(ctx, t1.ID, database.StatusDone) // done locally; unchanged in Google → kept
	if _, err := e.s.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if t1, _ = db.GetNodeBySource(ctx, "gtasks", "T1"); t1.Status != database.StatusDone {
		t.Fatal("local completion overwritten by an unchanged Google task")
	}
	if !strings.Contains(e.fake.queries["tasks.googleapis.com/tasks/v1/lists/L1/tasks"], "updatedMin=") {
		t.Fatal("incremental tasks sync not used")
	}

	if n, err := e.s.SyncDrive(ctx); err != nil || n != 2 {
		t.Fatalf("drive = %d, %v", n, err)
	}
	doc, err := db.GetNodeBySource(ctx, "drive", "doc1")
	if err != nil || !strings.Contains(doc.Content, "viajar para o Japão") || !strings.Contains(doc.Content, "**Dono:** Eu") {
		t.Fatalf("drive doc = %+v, %v", doc, err)
	}
	if cur, _, _ := db.KVGet(ctx, driveCursorKey); cur != "2026-09-03T10:00:00.000Z" {
		t.Fatalf("drive cursor = %q (skipped big file must not block)", cur)
	}
	if !strings.Contains(e.fake.queries["www.googleapis.com/drive/v3/files"], "modifiedTime") {
		t.Fatal("drive query without modifiedTime filter")
	}

	if n, err := e.s.SyncYouTube(ctx); err != nil || n != 3 {
		t.Fatalf("youtube = %d, %v", n, err)
	}
	if pl, err := db.GetNodeBySource(ctx, "youtube", "playlists"); err != nil || !strings.Contains(pl.Content, "Aula 1 — Canal Dev") {
		t.Fatalf("playlists = %+v, %v", pl, err)
	}
	if n, _ := e.s.SyncYouTube(ctx); n != 0 {
		t.Fatalf("second youtube sync wrote %d", n)
	}
}

func TestEmailDriveContactsTools(t *testing.T) {
	e := setup(t, allScopes(), nil)
	ctx := context.Background()
	mails, err := e.c.SearchEmail(ctx, "from:ana", 5)
	if err != nil || len(mails) != 1 || mails[0].Subject != "Orçamento" || mails[0].From != "Ana Lima <ana@x.com>" {
		t.Fatalf("search = %+v, %v", mails, err)
	}
	d, err := e.c.CreateDraft(ctx, agent.EmailDraft{ReplyTo: "m1", Body: "Olá Ana,\nsegue a planilha."})
	if err != nil || d.ThreadID != "t1" || !strings.Contains(d.Link, "msg9") {
		t.Fatalf("draft = %+v, %v", d, err)
	}
	msg := e.fake.drafts[0]["message"].(map[string]any)
	raw, _ := base64.URLEncoding.DecodeString(msg["raw"].(string))
	for _, want := range []string{"To: \"Ana Lima\" <ana@x.com>\r\n", "In-Reply-To: <abc@mail>\r\n", "References: <abc@mail>\r\n", "Subject: =?utf-8?q?Re:_Or=C3=A7amento?=\r\n"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("raw missing %q:\n%s", want, raw)
		}
	}
	if msg["threadId"] != "t1" {
		t.Fatal("reply not threaded")
	}
	_, body, _ := strings.Cut(string(raw), "\r\n\r\n")
	if dec, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(body, "\r\n", "")); string(dec) != "Olá Ana,\nsegue a planilha." {
		t.Fatalf("body = %q", dec)
	}
	if _, err := e.c.CreateDraft(ctx, agent.EmailDraft{To: "not an address", Body: "x"}); err == nil {
		t.Fatal("invalid recipient accepted")
	}

	people, err := e.c.SearchContacts(ctx, "ana", 10)
	if err != nil || len(people) != 2 || people[0].Name != "Ana Lima" || people[1].Emails[0] != "bruno@y.com" || !people[1].Other {
		t.Fatalf("contacts = %+v, %v", people, err)
	}
	files, err := e.c.SearchDrive(ctx, "plano d'água", 5)
	if err != nil || len(files) != 3 || !strings.Contains(e.fake.queries["www.googleapis.com/drive/v3/files"], url.QueryEscape(`'plano d\'água'`)) {
		t.Fatalf("drive search = %v, %v, %s", files, err, e.fake.queries["www.googleapis.com/drive/v3/files"])
	}
	_, _, err = e.c.ReadDriveFile(ctx, "off")
	var ae *APIError
	if !queue.IsPermanent(err) || !errors.As(err, &ae) || !strings.Contains(ae.Message, "ative a Google Drive API") {
		t.Fatalf("disabled API error = %v", err)
	}
}

func TestClassifyAndHelpers(t *testing.T) {
	if err := classify(gmailAPI, 429, nil); queue.IsPermanent(err) {
		t.Fatal("429 must be retryable")
	}
	if err := classify(driveAPI, 404, []byte(`{"error":{"message":"File not found"}}`)); !queue.IsPermanent(err) || !isStatus(err, 404) {
		t.Fatalf("404 = %v", err)
	}
	if err := classify(youtubeAPI, 403, []byte(`{"error":{"errors":[{"reason":"quotaExceeded"}]}}`)); queue.IsPermanent(err) {
		t.Fatal("quota errors must be retried later")
	}
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	if d := dueDate("2026-10-01T00:00:00.000Z", loc); d == nil || d.Format("2006-01-02 15:04 MST") != "2026-10-01 00:00 -03" {
		t.Fatalf("due = %v", d)
	}
	raw := buildRaw("a@b.c", "", "Linha\r\nBcc: x@y.z", "oi", "", "")
	if strings.Contains(raw, "\r\nBcc:") {
		t.Fatal("header injection")
	}
	if got := replySubject("RE: x"); got != "RE: x" {
		t.Fatal(got)
	}
}

func TestSyncTakeoutFromDrive(t *testing.T) {
	e := setup(t, allScopes(), nil)
	ctx := context.Background()
	tf := func(id, name, created string) map[string]any {
		return map[string]any{"id": id, "name": name, "mimeType": "application/zip", "createdTime": created, "size": "8"}
	}
	e.fake.takeouts = []map[string]any{
		tf("old1", "takeout-20260701T100000Z-001.zip", "2026-07-01T10:00:00.000Z"),
		tf("old2", "takeout-20260701T100000Z-002.zip", "2026-07-01T10:01:00.000Z"),
		tf("new1", "takeout-20260901T100000Z-001.zip", "2026-09-01T10:00:00.000Z"),
		tf("new2", "takeout-20260901T100000Z-002.zip", "2026-09-01T10:01:00.000Z"),
	}
	var mu sync.Mutex
	var queued []string
	e.s.ImportFile = func(_ context.Context, p, name string) error {
		mu.Lock()
		defer mu.Unlock()
		b, _ := os.ReadFile(p)
		queued = append(queued, name+"="+string(b))
		return nil
	}
	if n, err := e.s.SyncTakeout(ctx); err != nil || n != 2 {
		t.Fatalf("first run = %d, %v", n, err)
	}
	sorted := strings.Join(queued, ",")
	if sorted != "takeout-20260901T100000Z-001.zip=zip-new1,takeout-20260901T100000Z-002.zip=zip-new2" &&
		sorted != "takeout-20260901T100000Z-002.zip=zip-new2,takeout-20260901T100000Z-001.zip=zip-new1" {
		t.Fatalf("first run queued %v (only the latest export expected)", queued)
	}
	queued = nil
	e.fake.takeouts = append(e.fake.takeouts, tf("next1", "takeout-20261101T100000Z-001.zip", "2026-11-01T10:00:00.000Z"))
	if n, err := e.s.SyncTakeout(ctx); err != nil || n != 1 || len(queued) != 1 || !strings.HasSuffix(queued[0], "=zip-next1") {
		t.Fatalf("second run = %d, %v, %v", n, err, queued)
	}
	if n, _ := e.s.SyncTakeout(ctx); n != 0 {
		t.Fatal("already imported export fetched again")
	}
}
