package profile

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
)

func newStore(t *testing.T) (*Store, *config.Config, *database.DB) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(map[string]string{"TIMEZONE": "America/Sao_Paulo", "BACKUP_ENCRYPTION_KEY": "backup-key-test"}); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(cfg, db, slog.New(slog.NewTextHandler(os.Stderr, nil))), cfg, db
}

func TestSensitiveItemsAreEncrypted(t *testing.T) {
	s, cfg, db := newStore(t)
	ctx := context.Background()
	it := Item{Kind: "medication", Title: "Losartana", Sensitive: true, Values: map[string]string{"dose": "50 mg", "times": "20h, 8", "stock": "30"}}
	if err := s.Save(ctx, &it); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetProfile(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.Title+row.Data, "Losartana") || !strings.HasPrefix(row.Data, encPrefix) {
		t.Fatalf("sensitive item stored in clear: %+v", row)
	}
	if cfg.Get("VAULT_KEY") == "" {
		t.Fatal("VAULT_KEY not created")
	}
	got, err := s.Get(ctx, it.ID)
	if err != nil || got.Title != "Losartana" || got.Get("times") != "08:00, 20:00" {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}

	// Lost .env: the key comes back from the copy wrapped with the backup key.
	key := cfg.Get("VAULT_KEY")
	if err := cfg.Delete("VAULT_KEY"); err != nil {
		t.Fatal(err)
	}
	s2 := New(cfg, db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if got, err := s2.Get(ctx, it.ID); err != nil || got.Title != "Losartana" || cfg.Get("VAULT_KEY") != key {
		t.Fatalf("recovery from wrapped key failed: %+v %v", got, err)
	}

	// Wrong key: the item is locked, not garbage.
	if err := cfg.Update(map[string]string{"VAULT_KEY": strings.Repeat("ab", 32)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, it.ID); err != ErrLocked {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

func TestSaveValidation(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	for _, it := range []Item{
		{Kind: "nope", Title: "x"},
		{Kind: "enrollment", Title: ""},
		{Kind: "enrollment", Title: "Academia", Values: map[string]string{"time": "25:00"}},
		{Kind: "bill", Title: "Luz", Values: map[string]string{"due_day": "40"}},
		{Kind: "document", Title: "CNH", Values: map[string]string{"expires": "31/12/2030"}},
	} {
		if err := s.Save(ctx, &it); err == nil {
			t.Fatalf("expected error for %+v", it)
		}
	}
}

func TestTodayUpcomingAndCheck(t *testing.T) {
	s, cfg, _ := newStore(t)
	ctx := context.Background()
	loc := cfg.Location()
	now := time.Date(2026, 9, 29, 7, 30, 0, 0, loc) // Tuesday
	save := func(it Item) Item {
		t.Helper()
		if err := s.Save(ctx, &it); err != nil {
			t.Fatal(err)
		}
		return it
	}
	med := save(Item{Kind: "medication", Title: "Losartana", Sensitive: true, Values: map[string]string{"times": "08:00, 20:00", "stock": "8", "prescription": "2026-10-05"}})
	save(Item{Kind: "enrollment", Title: "Academia", Values: map[string]string{"weekdays": "seg,qua,sex", "time": "18:00"}})
	save(Item{Kind: "enrollment", Title: "Inglês", Values: map[string]string{"weekdays": "ter", "time": "19:00", "due_day": "1", "fee": "300"}})
	save(Item{Kind: "habit", Title: "Água", Values: map[string]string{"target": "2 L"}})
	save(Item{Kind: "date", Title: "Casamento", Values: map[string]string{"date": "2016-10-10", "category": "casamento/namoro", "remind": "20"}})
	save(Item{Kind: "document", Title: "CNH", Sensitive: true, Values: map[string]string{"expires": "2026-09-20"}})
	save(Item{Kind: "doctor", Title: "Dra. Ana", Values: map[string]string{"last": "2025-09-01", "every": "12"}})
	save(Item{Kind: "surgery", Title: "Joelho", Values: map[string]string{"date": "2026-01-10"}}) // past event: not listed

	items, err := s.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	today := Today(items, midnight(now), nil)
	var titles []string
	for _, sl := range today {
		titles = append(titles, sl.Time+" "+sl.Title)
	}
	if got := strings.Join(titles, "|"); got != "08:00 Losartana|19:00 Inglês|20:00 Losartana| Água" {
		t.Fatalf("today: %s", got)
	}

	alerts := Upcoming(items, now, 30)
	find := func(label string) *Alert {
		for i := range alerts {
			if strings.Contains(alerts[i].Label, label) {
				return &alerts[i]
			}
		}
		return nil
	}
	if a := find("Receita vence"); a == nil || a.Days != 6 || a.Section != "saude" {
		t.Fatalf("prescription alert: %+v", a)
	}
	if a := find("Estoque"); a == nil || a.Level != LevelWarn {
		t.Fatalf("stock alert (8 units / 2 per day): %+v", a)
	}
	if a := find("Documento vence"); a == nil || a.Level != LevelLate || a.Days != -9 {
		t.Fatalf("expired document: %+v", a)
	}
	if a := find("Consulta de rotina"); a == nil || a.Days != -28 || a.Level != LevelLate {
		t.Fatalf("checkup: %+v", a)
	}
	if a := find("Casamento"); a == nil || a.Title != "Casamento (10 anos)" || a.Days != 11 {
		t.Fatalf("anniversary: %+v", a)
	}
	if a := find("Pagamento"); a == nil || a.Days != 2 || a.Label != "Pagamento R$ 300" {
		t.Fatalf("monthly payment: %+v", a)
	}
	if find("Cirurgia") != nil {
		t.Fatal("past surgery should not be listed")
	}

	// Checking a dose decrements the stock once; undo restores it.
	it, slot, changed, err := s.CheckNow(ctx, med.ID, now)
	if err != nil || !changed || slot != "08:00" || it.Get("stock") != "7" {
		t.Fatalf("check: %+v %s %v %v", it, slot, changed, err)
	}
	if _, _, changed, _ := s.CheckNow(ctx, med.ID, now); changed { // 20:00 is not due yet
		t.Fatal("second check at the same time should not change anything")
	}
	if it, _, _ := s.Check(ctx, med.ID, now, "08:00", true); it.Get("stock") != "8" {
		t.Fatalf("undo stock: %s", it.Get("stock"))
	}
}

func TestBirthdaysClassifyAndMerge(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, loc)
	b := Birthdays([]Person{{ID: 1, Name: "Vó", Birthday: "1940-09-30"}, {ID: 2, Name: "Ana", Birthday: "03-12"}, {ID: 3, Name: "X", Birthday: "lixo"}}, now, 30)
	if len(b) != 1 || b[0].Title != "Vó (86 anos)" || b[0].Days != 1 {
		t.Fatalf("birthdays: %+v", b)
	}
	for title, kind := range map[string]string{"Cirurgia no joelho": "surgery", "Consulta Dra. Paula": "doctor", "Voo GRU → LIS": "trip", "Aula de Python": "course", "Aniversário da vó": "date", "Reunião semanal": ""} {
		c, ok := Classify(title)
		if (kind == "") == ok || c.Kind != kind {
			t.Fatalf("classify %q = %+v %v", title, c, ok)
		}
	}
	evs := []Event{
		{ID: "e1", Title: "Aniversário da Vó", Start: time.Date(2026, 9, 30, 0, 0, 0, 0, loc), AllDay: true},
		{ID: "e2", Title: "Cirurgia no joelho", Start: time.Date(2026, 10, 5, 7, 0, 0, 0, loc)},
		{ID: "e3", Title: "Reunião semanal", Start: time.Date(2026, 10, 1, 9, 0, 0, 0, loc)},
		{ID: "e4", Title: "Vacina gripe", Start: time.Date(2026, 10, 2, 9, 0, 0, 0, loc)},
	}
	all := Merge(b, EventAlerts(evs, now, map[string]bool{"e4": true}))
	if len(all) != 2 || all[0].Source != "contatos" || !strings.HasPrefix(all[1].Title, "Cirurgia no joelho · 07:00") {
		t.Fatalf("merge: %+v", all)
	}
	sug := Suggest(append(evs, Event{ID: "e5", Title: "Cirurgia no joelho", Start: time.Date(2026, 10, 12, 7, 0, 0, 0, loc)}), map[string]bool{"e1": true}, loc)
	if len(sug) != 2 || sug[0].Event.ID != "e4" || sug[1].Event.ID != "e2" { // by date, repeated titles once
		t.Fatalf("suggest: %+v", sug)
	}
	it := sug[1].Item()
	if it.Kind != "surgery" || it.Get("date") != "2026-10-05" || !it.Sensitive {
		t.Fatalf("suggestion item: %+v", it)
	}
	if d := Digest("📋 *Hoje*", []Slot{{ItemID: 3, Icon: "💊", Time: "08:00", Title: "Losartana", Check: true}}, all); !strings.Contains(d, "/tomei 3") || !strings.Contains(d, "amanhã") {
		t.Fatalf("digest: %s", d)
	}
}

func TestForAIPolicy(t *testing.T) {
	s, cfg, _ := newStore(t)
	ctx := context.Background()
	for _, it := range []Item{
		{Kind: "medication", Title: "Losartana", Sensitive: true},
		{Kind: "enrollment", Title: "Academia Forte", Values: map[string]string{"time": "18:00"}},
	} {
		if err := s.Save(ctx, &it); err != nil {
			t.Fatal(err)
		}
	}
	count := func(local bool) (int, bool) {
		out, err := s.ForAI(ctx, "", "", local)
		if err != nil {
			t.Fatal(err)
		}
		items, _ := out["items"].([]map[string]any)
		_, hidden := out["hidden_sensitive"]
		return len(items), hidden
	}
	if n, hidden := count(false); n != 1 || !hidden {
		t.Fatalf("basic+cloud: %d %v", n, hidden)
	}
	if n, _ := count(true); n != 2 {
		t.Fatalf("basic+local: %d", n)
	}
	cfg.Update(map[string]string{"PROFILE_AI_ACCESS": AccessFull})
	if n, _ := count(false); n != 2 {
		t.Fatalf("full: %d", n)
	}
	cfg.Update(map[string]string{"PROFILE_AI_ACCESS": AccessNone})
	if out, _ := s.ForAI(ctx, "", "", true); out["items"] != nil {
		t.Fatalf("none: %+v", out)
	}
}

func TestConcurrentKeyCreation(t *testing.T) {
	s, cfg, _ := newStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			it := Item{Kind: "document", Title: "RG", Sensitive: true}
			if err := s.Save(ctx, &it); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	items, _ := s.List(ctx, false)
	if len(items) != 8 || cfg.Get("VAULT_KEY") == "" {
		t.Fatalf("items=%d", len(items))
	}
	for _, it := range items {
		if it.Locked {
			t.Fatal("item encrypted with a different key")
		}
	}
}
