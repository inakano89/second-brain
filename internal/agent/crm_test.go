package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/profile"
)

func TestCRMStaleContactsAndBrief(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	now := time.Now()
	mk := func(n *database.Node) *database.Node {
		t.Helper()
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	person := func(title string, tags ...string) *database.Node {
		return mk(&database.Node{Type: database.TypePerson, Title: title, Tags: tags, Meta: map[string]any{"emails": []string{strings.ToLower(title) + "@x.com"}}})
	}
	talk := func(p *database.Node, title string, daysAgo int) *database.Node {
		n := mk(&database.Node{Type: database.TypeNote, Title: title, Content: "conversa", Source: "web", CreatedAt: now.AddDate(0, 0, -daysAgo)})
		db.AddEdge(ctx, n.ID, p.ID, "mentions", 1)
		return n
	}
	ana := person("Ana")
	for i, d := range []int{200, 150, 120, 100} { // frequent, last talked 100 days ago
		talk(ana, "Papo com Ana "+string(rune('A'+i)), d)
	}
	bia := person("Bia")
	talk(bia, "Só uma vez", 300) // one interaction, no tag: not a candidate
	caio := person("Caio", "crm")
	talk(caio, "Café com Caio", 30) // opted in but recent
	duda := person("Duda", "crm")
	talk(duda, "Almoço com Duda", 95)
	edu := person("Edu", "sem-crm")
	for _, d := range []int{400, 300, 200, 150} {
		talk(edu, "Edu", d)
	}
	fabi := person("Fabi")
	for _, d := range []int{500, 400, 300, 200} {
		talk(fabi, "Fabi", d)
	}
	future := now.AddDate(0, 0, 5)
	ev := mk(&database.Node{Type: database.TypeEvent, Title: "Jantar com Fabi", Source: "calendar", DueAt: &future})
	db.AddEdge(ctx, ev.ID, fabi.ID, "attendee", 1)
	// Imported notes and undated items are not interactions.
	imp := mk(&database.Node{Type: database.TypeNote, Title: "Importada", Source: "import:markdown", CreatedAt: now.AddDate(0, 0, -1)})
	db.AddEdge(ctx, imp.ID, ana.ID, "mentions", 1)

	list, err := a.StaleContacts(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range list {
		names = append(names, c.Person.Title)
	}
	if strings.Join(names, ",") != "Ana,Duda" { // Ana 100d/90 = 1.11, Duda 95/90 = 1.05
		t.Fatalf("stale = %v", names)
	}
	text := FormatStaleContacts(list, a.cfg.Location(), now)
	if !strings.Contains(text, "*Ana* — há 3 meses") || !strings.Contains(text, "/pessoa") {
		t.Fatalf("text = %q", text)
	}
	// Each silence is reported once.
	first, _ := a.CRMReminders(ctx, now, 5)
	second, _ := a.CRMReminders(ctx, now, 5)
	if len(first) != 2 || len(second) != 0 {
		t.Fatalf("reminders = %d then %d", len(first), len(second))
	}
	// A per-person threshold and the global switch.
	caio.Meta[metaContactEvery] = float64(20)
	db.UpdateNode(ctx, caio)
	if list, _ := a.StaleContacts(ctx, now, 10); len(list) != 3 {
		t.Fatalf("with contact_every_days: %d", len(list))
	}
	a.cfg.Update(map[string]string{"CRM_STALE_DAYS": "0"})
	if list, _ := a.StaleContacts(ctx, now, 10); list != nil {
		t.Fatalf("off: %v", list)
	}

	// Brief.
	task := mk(&database.Node{Type: database.TypeTask, Title: "Mandar proposta para Ana", Status: database.StatusOpen})
	db.AddEdge(ctx, task.ID, ana.ID, "mentions", 1)
	b, err := a.PersonBriefOf(ctx, ana.ID, now)
	if err != nil || b.Count != 4 || len(b.OpenTasks) != 1 || b.LastTitle != "Papo com Ana D" {
		t.Fatalf("brief = %+v, %v", b, err)
	}
	out := FormatPersonBrief(b, a.cfg.Location(), now)
	if !strings.Contains(out, "Última interação há 3 meses") || !strings.Contains(out, "☑️ #") {
		t.Fatalf("brief text = %q", out)
	}
	if _, err := a.PersonBriefOf(ctx, task.ID, now); err == nil {
		t.Fatal("a task is not a person")
	}
	if p, err := a.FindPerson(ctx, "duda"); err != nil || p.ID != duda.ID {
		t.Fatalf("find = %v %v", p, err)
	}
	if _, err := a.FindPerson(ctx, "ninguém"); err == nil {
		t.Fatal("unknown person should fail")
	}
}

func TestMeetingPrep(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	now := time.Now()
	ana := &database.Node{Type: database.TypePerson, Title: "Ana Lima", Meta: map[string]any{"emails": []string{"ana@x.com"}}}
	db.CreateNode(ctx, ana)
	past := &database.Node{Type: database.TypeNote, Title: "Última conversa", Source: "web", CreatedAt: now.AddDate(0, 0, -21)}
	db.CreateNode(ctx, past)
	db.AddEdge(ctx, past.ID, ana.ID, "mentions", 1)
	soon := now.Add(30 * time.Minute)
	mk := func(title, ref string, at time.Time, attendees []string) {
		db.CreateNode(ctx, &database.Node{Type: database.TypeEvent, Title: title, Source: "calendar", SourceRef: ref, DueAt: &at, Meta: map[string]any{"attendees": attendees}})
	}
	mk("Alinhamento Atlas", "e1", soon, []string{"ana@x.com", "desconhecido@y.com"})
	mk("Sem convidados", "e2", soon, nil)
	mk("Convidado desconhecido", "e3", soon, []string{"zzz@y.com"})
	mk("Muito depois", "e4", now.Add(5*time.Hour), []string{"ana@x.com"})

	out, err := a.MeetingPrep(ctx, now, 45*time.Minute)
	if err != nil || len(out) != 1 || !strings.Contains(out[0], "Alinhamento Atlas") || !strings.Contains(out[0], "Ana Lima") || !strings.Contains(out[0], "3 semanas") {
		t.Fatalf("prep = %q, %v", out, err)
	}
	if out, _ := a.MeetingPrep(ctx, now, 45*time.Minute); len(out) != 0 {
		t.Fatalf("reported twice: %q", out)
	}
}

func TestTravelDossier(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	loc := a.cfg.Location()
	now := time.Now().In(loc)
	day := func(n int) string { return now.AddDate(0, 0, n).Format("2006-01-02") }
	save := func(it profile.Item) {
		t.Helper()
		if err := a.profile.Save(ctx, &it); err != nil {
			t.Fatal(err)
		}
	}
	save(profile.Item{Kind: "trip", Title: "Lisboa", Values: map[string]string{"start": day(5), "end": day(12), "bookings": "Voo TP 88\nHotel Baixa", "checklist": "Carregador\nAdaptador"}})
	save(profile.Item{Kind: "document", Title: "Passaporte", Sensitive: true, Values: map[string]string{"expires": day(12 + 90)}})
	save(profile.Item{Kind: "medication", Title: "Losartana", Sensitive: true, Values: map[string]string{"times": "08:00", "stock": "3"}})
	db.CreateNode(ctx, &database.Node{Type: database.TypeNote, Title: "Restaurantes em Lisboa", Content: "Time Out Market", Tags: []string{"viagem"}})
	db.CreateNode(ctx, &database.Node{Type: database.TypeTask, Title: "Comprar passagem de trem", Status: database.StatusOpen})
	start := now.AddDate(0, 0, 8)
	db.CreateNode(ctx, &database.Node{Type: database.TypeEvent, Title: "Dentista", Source: "calendar", SourceRef: "d1", DueAt: &start})

	text, err := a.TravelDossier(ctx, travelOpts("Lisboa", true, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"✈️ *Dossiê: Lisboa*", "8 dias", "⚠️", "Dentista", "Restaurantes em Lisboa", "Comprar passagem de trem",
		"*Passaporte* vence em", "menos de 6 meses", "*Losartana*: estoque para ~3 dias", "Voo TP 88", "Adaptador"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	// Without permission to read sensitive items only the trip item shows up.
	safe, _ := a.TravelDossier(ctx, TravelOptions{Destination: "Lisboa", Personal: true})
	if strings.Contains(safe, "Passaporte") || strings.Contains(safe, "Losartana") {
		t.Errorf("sensitive profile items leaked:\n%s", safe)
	}
	// The chat tool follows PROFILE_AI_ACCESS.
	res, err := a.travelForChat(ctx, toolArgs{"destination": "Lisboa"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if strings.Contains(m["dossier"].(string), "Passaporte") || m["note"] == nil {
		t.Errorf("chat tool leaked sensitive items: %v", m)
	}
	a.cfg.Update(map[string]string{"PROFILE_AI_ACCESS": "none"})
	res, _ = a.travelForChat(ctx, toolArgs{"destination": "Lisboa"})
	if strings.Contains(res.(map[string]any)["dossier"].(string), "Voo TP 88") {
		t.Error("PROFILE_AI_ACCESS=none must hide the profile")
	}
	if trips := a.UpcomingTrips(ctx, now, 7); len(trips) != 1 {
		t.Fatalf("upcoming = %d", len(trips))
	}
	if trips := a.UpcomingTrips(ctx, now, 3); len(trips) != 0 {
		t.Fatalf("upcoming in 3 days = %d", len(trips))
	}
	if _, err := a.TravelDossier(ctx, TravelOptions{}); err != nil { // next trip of the profile
		t.Fatalf("no destination: %v", err)
	}
}

func travelOpts(dest string, personal, sensitive bool) TravelOptions {
	return TravelOptions{Destination: dest, Personal: personal, SensitiveOK: sensitive}
}
