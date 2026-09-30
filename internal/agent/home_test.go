package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/profile"
)

func TestReceiptToWarranty(t *testing.T) {
	ctx := context.Background()
	sp, _ := time.LoadLocation("America/Sao_Paulo") // the default TIMEZONE of the tests' config
	today := time.Now().In(sp).Format("2006-01-02")
	f := &fakeLLM{answer: func(system, user string) string {
		if !strings.Contains(system, "nota fiscal, recibo ou cupom") {
			return "{}"
		}
		return `{"store":"Casas Bahia","date":"` + today + `","total":3499.9,"invoice":"12345","durable_items":[
			{"product":"Geladeira Frost Free 400L","brand":"Brastemp","price":3299.9,"warranty_months":24},
			{"product":"Cabo HDMI","brand":"","price":29,"warranty_months":0}]}`
	}}
	a, db := setupAgent(t, f)
	mk := func(title, content string, tags ...string) *database.Node {
		n := &database.Node{Type: database.TypeNote, Title: title, Content: content, Tags: tags, CreatedAt: time.Now()}
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	n := mk("Nota Casas Bahia", "NF-e 12345 Geladeira Brastemp 24 meses de garantia", "recibo")

	// Without permission to write the profile the user is told what was found.
	msg, err := a.ReceiptWarranties(ctx, n)
	if err != nil || !strings.Contains(msg, "PROFILE_AI_WRITE") || !strings.Contains(msg, "Brastemp Geladeira Frost Free 400L") {
		t.Fatalf("read-only = %q, %v", msg, err)
	}
	if items, _ := a.profile.List(ctx, false); len(items) != 0 {
		t.Fatalf("profile written without permission: %+v", items)
	}
	a.cfg.Update(map[string]string{"PROFILE_AI_WRITE": "true"})
	msg, err = a.ReceiptWarranties(ctx, n)
	if err != nil || !strings.Contains(msg, "2 garantia(s) cadastrada(s)") || !strings.Contains(msg, "24 meses") || !strings.Contains(msg, "12 meses, presumido") {
		t.Fatalf("write = %q, %v", msg, err)
	}
	items, _ := a.profile.List(ctx, false)
	byTitle := map[string]profile.Item{}
	for _, it := range items {
		byTitle[it.Title] = it
	}
	fr := byTitle["Brastemp Geladeira Frost Free 400L"]
	wantUntil := time.Now().In(sp).AddDate(2, 0, 0).Format("2006-01-02")
	if fr.Kind != "warranty" || fr.Get("until") != wantUntil || fr.Get("store") != "Casas Bahia" ||
		!strings.Contains(fr.Get("notes"), "Nota fiscal 12345") || !strings.Contains(fr.Get("notes"), "R$ 3299.90") {
		t.Fatalf("item = %+v", fr)
	}
	if !strings.Contains(byTitle["Cabo HDMI"].Get("notes"), "presumido") {
		t.Fatalf("assumed term not flagged: %+v", byTitle["Cabo HDMI"])
	}
	// Running it again updates instead of duplicating.
	a.ReceiptWarranties(ctx, n)
	if items, _ := a.profile.List(ctx, false); len(items) != 2 {
		t.Fatalf("duplicated warranties: %d", len(items))
	}
	// The warranty shows up in the upcoming dates.
	alerts := profile.Upcoming(items, time.Now(), 800)
	found := false
	for _, al := range alerts {
		if strings.Contains(al.Label, "Garantia") && strings.Contains(al.Title, "Geladeira") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no expiry alert in %+v", alerts)
	}

	// Only purchase documents are read.
	if looksLikeReceipt("foto da sala", "IMG_1.jpg", &database.Node{}) {
		t.Error("plain photo taken as receipt")
	}
	for _, c := range [][2]string{{"garantia da tv", "a.jpg"}, {"", "nota-fiscal-tv.pdf"}, {"", "danfe.pdf"}} {
		if !looksLikeReceipt(c[0], c[1], &database.Node{}) {
			t.Errorf("missed receipt: %v", c)
		}
	}
	if !looksLikeReceipt("", "x.jpg", &database.Node{Tags: []string{"nota-fiscal"}}) {
		t.Error("tag not used")
	}
}

func TestHealthRoutineAnalysis(t *testing.T) {
	loc := time.UTC
	var loads []DayLoad
	metrics := map[string]map[string]float64{"sleep_minutes": {}, "recovery_score": {}}
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	for i := 0; i < 20; i++ {
		d := day.AddDate(0, 0, i)
		hours := 0.5
		if i%2 == 0 {
			hours = 6
		}
		loads = append(loads, DayLoad{Date: d.Format("2006-01-02"), Hours: hours, Meetings: 3})
		next := d.AddDate(0, 0, 1).Format("2006-01-02")
		metrics["sleep_minutes"][next] = 420 - hours*10 // busy day: 360, light: 415
		metrics["recovery_score"][next] = 80 - hours*3
	}
	rep := AnalyzeRoutine(loads, metrics, loc)
	if rep.Busy.Days != 10 || rep.Light.Days != 10 {
		t.Fatalf("groups = %d/%d", rep.Busy.Days, rep.Light.Days)
	}
	if got := rep.Busy.Avg["sleep_minutes"]; got != 360 {
		t.Errorf("busy sleep = %v", got)
	}
	if got := rep.Light.Avg["sleep_minutes"]; got != 415 {
		t.Errorf("light sleep = %v", got)
	}
	if r := rep.Corr["sleep_minutes"]; r > -0.99 {
		t.Errorf("correlation = %v, want -1", r)
	}
	out := rep.Format(30)
	for _, want := range []string{"Dias cheios", "sono 6h00", "sono 6h55", "dorme em média 55 min a menos", "recuperação cai", "forte"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if sparse := AnalyzeRoutine(loads[:4], metrics, loc).Format(30); !strings.Contains(sparse, "Poucos dias") {
		t.Errorf("sparse = %q", sparse)
	}
	if (RoutineReport{}).Format(30) != "" {
		t.Error("empty report should print nothing")
	}
}

func TestMeetingLoadsAndHealthRoutine(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	loc := a.cfg.Location()
	today := time.Date(time.Now().In(loc).Year(), time.Now().In(loc).Month(), time.Now().In(loc).Day(), 0, 0, 0, 0, loc)
	// Events cross midnight and all-day events do not count.
	night := time.Date(2026, 9, 1, 22, 0, 0, 0, loc)
	events := []CalendarEvent{
		{Start: night, End: night.Add(4 * time.Hour)},
		{Start: night, End: night.Add(24 * time.Hour), AllDay: true},
	}
	loads := meetingLoads(events, night.AddDate(0, 0, -1), night.AddDate(0, 0, 3), loc)
	got := map[string]float64{}
	for _, l := range loads {
		got[l.Date] = l.Hours
	}
	if len(loads) != 4 || got["2026-09-01"] != 2 || got["2026-09-02"] != 2 || got["2026-08-31"] != 0 {
		t.Fatalf("loads = %+v", loads)
	}
	if meetingLoads(nil, today.AddDate(0, 0, -5), today, loc) != nil {
		t.Error("no calendar data should give no days")
	}

	// End to end with event nodes and metrics.
	for i := 1; i <= 12; i++ {
		d := today.AddDate(0, 0, -i)
		hours := 1
		if i%2 == 0 {
			hours = 5
		}
		start := d.Add(9 * time.Hour)
		db.CreateNode(ctx, &database.Node{Type: database.TypeEvent, Title: "Reunião", Source: "calendar", SourceRef: d.Format("060102"), DueAt: &start,
			Meta: map[string]any{"end": start.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339)}})
		db.UpsertMetric(ctx, database.Metric{Date: d.AddDate(0, 0, 1).Format("2006-01-02"), Kind: "sleep_minutes", Value: float64(440 - hours*20), Source: "test"})
	}
	rep, text, err := a.HealthRoutine(ctx, 14)
	if err != nil || rep.Busy.Days == 0 || !strings.Contains(text, "Saúde × rotina") {
		t.Fatalf("routine = %+v %q %v", rep, text, err)
	}
}

func TestShoppingList(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	if l, err := a.ShoppingList(ctx); err != nil || l != nil {
		t.Fatalf("empty = %v, %v", l, err)
	}
	list, err := a.ShoppingEdit(ctx, "add", []string{"Leite", "pão", "ovos", "leite"})
	if err != nil || len(list) != 3 {
		t.Fatalf("add = %v, %v", list, err)
	}
	if _, err := a.ShoppingEdit(ctx, "check", []string{"pao"}); err == nil { // no accent folding: reported, not guessed
		t.Fatal("unknown item should fail")
	}
	list, err = a.ShoppingEdit(ctx, "check", []string{"pão", "ov"}) // fragment "ov" is unique
	if err != nil || !list[1].Done || !list[2].Done || list[0].Done {
		t.Fatalf("check = %v, %v", list, err)
	}
	list, _ = a.ShoppingEdit(ctx, "add", []string{"Pão"}) // buying again reopens it
	if list[1].Done || len(list) != 3 {
		t.Fatalf("re-add = %v", list)
	}
	list, _ = a.ShoppingEdit(ctx, "clear_done", nil)
	if len(list) != 2 || list[0].Text != "Leite" || list[1].Text != "pão" {
		t.Fatalf("clear_done = %v", list)
	}
	n, err := db.GetNodeBySource(ctx, "shopping", "list:default")
	if err != nil || !strings.Contains(n.Content, "- [ ] Leite") || n.Title != "Lista de compras" {
		t.Fatalf("node = %+v, %v", n, err)
	}
	if got := FormatShopping(list); !strings.Contains(got, "⬜ Leite") {
		t.Fatalf("format = %q", got)
	}
	// The chat tool asks before changing the list; reading needs no permission.
	res := a.ExecuteTool(ctx, toolCall(toolShoppingEdit, `{"action":"clear_all"}`))
	if !strings.Contains(res, "needs_confirmation") {
		t.Fatalf("edit without permission = %s", res)
	}
	if l, _ := a.ShoppingList(ctx); len(l) != 2 {
		t.Fatal("list changed without permission")
	}
	res = a.ExecuteTool(ctx, toolCall(toolShoppingEdit, `{"action":"clear_all","user_requested":true}`))
	if strings.Contains(res, "needs_confirmation") || strings.Contains(res, "error") {
		t.Fatalf("edit = %s", res)
	}
	res = a.ExecuteTool(ctx, toolCall(toolShoppingList, `{}`))
	if strings.Contains(res, "Leite") {
		t.Fatalf("list after clear_all = %s", res)
	}
}

func toolCall(name, args string) llm.ToolCall {
	return llm.ToolCall{ID: "t1", Name: name, Arguments: json.RawMessage(args)}
}
