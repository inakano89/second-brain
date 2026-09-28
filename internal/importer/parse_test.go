package importer

import (
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/database"
)

var sp = func() *time.Location { l, _ := time.LoadLocation("America/Sao_Paulo"); return l }()

func newTestParser() *parser { return newParser(sp, 10<<20) }

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func TestDetect(t *testing.T) {
	cases := map[string][2]string{
		"a.enex":           {"", FormatEvernote},
		"contatos.vcf":     {"", FormatVCard},
		"agenda.ics":       {"", FormatICal},
		"feeds.opml":       {"", FormatOPML},
		"x.csv":            {"", FormatCSV},
		"n.md":             {"", FormatMarkdown},
		"keep.json":        {`{"isTrashed":false,"textContent":"x","userEditedTimestampUsec":1}`, FormatKeep},
		"api.json":         {`[{"title":"x"}]`, FormatJSON},
		"bookmarks.html":   {"<!DOCTYPE NETSCAPE-Bookmark-file-1>", FormatBookmarks},
		"page.html":        {"<html><body><p>oi</p></body></html>", FormatHTML},
		"My Clippings.txt": {"Livro (Autor)\n- Seu destaque", FormatKindle},
		"nota.txt":         {"texto simples", FormatMarkdown},
		"sem-extensao":     {"BEGIN:VCALENDAR\nEND:VCALENDAR", FormatICal},
		"export.xml":       {`<?xml version="1.0"?><en-export>`, FormatEvernote},
		"x.bin":            {"\x00\x01", ""},
	}
	for name, c := range cases {
		if got := detect(name, []byte(c[0])); got != c[1] {
			t.Errorf("detect(%q) = %q, want %q", name, got, c[1])
		}
	}
}

func TestMarkdownObsidianAndNotion(t *testing.T) {
	p := newTestParser()
	doc := "---\ntitle: \"Projeto \\\"Atlas\\\"\"\ntags:\n  - trabalho\n  - planejamento\naliases: [Atlas]\ncreated: 2024-03-10\n---\n# Projeto\nVer [[Maria Souza]] e [Orçamento](Or%C3%A7amento%202024%20aaaabbbbccccddddeeeeffff00001111.md). #urgente\n```\n#naotag\n```\n"
	it := p.markdownItem(FormatMarkdown, "Vault/Projetos/Atlas.md", doc, time.Time{})
	if it.Title != `Projeto "Atlas"` || !has(it.Tags, "trabalho") || !has(it.Tags, "planejamento") || !has(it.Tags, "urgente") || has(it.Tags, "naotag") {
		t.Fatalf("frontmatter/tags: %+v", it)
	}
	if !has(it.Aliases, "Atlas") || it.Ref != "Vault/Projetos/Atlas.md" || it.CreatedAt.Format("2006-01-02") != "2024-03-10" {
		t.Fatalf("aliases/ref/date: %+v", it)
	}
	if !strings.Contains(it.Content, "[[Orçamento 2024|Orçamento]]") || !strings.Contains(it.Content, "[[Maria Souza]]") {
		t.Fatalf("links not converted: %q", it.Content)
	}

	n := p.markdownItem(FormatNotion, "Export/Reunião semanal 0123456789abcdef0123456789abcdef.md", "# Reunião semanal\n\nStatus: Feito\n\nPauta", time.Time{})
	if n.Title != "Reunião semanal" || strings.HasPrefix(n.Content, "#") || n.Format != FormatNotion {
		t.Fatalf("notion: %+v", n)
	}

	lg := p.markdownItem(FormatMarkdown, "pages/ideia.md", "title:: Grande Ideia\ntags:: [[ia]], [[produto]]\n\n- bloco\n  id:: 65a1b2c3-0000\n- outro", time.Time{})
	if lg.Title != "Grande Ideia" || !has(lg.Tags, "ia") || !has(lg.Tags, "produto") || strings.Contains(lg.Content, "id::") {
		t.Fatalf("logseq: %+v", lg)
	}

	daily := p.markdownItem(FormatMarkdown, "journals/2024_01_15.md", "- dia", time.Time{})
	if daily.CreatedAt.Format("2006-01-02") != "2024-01-15" {
		t.Fatalf("daily note date: %v", daily.CreatedAt)
	}
}

func TestMarkdownOwnExportRoundTrip(t *testing.T) {
	doc := "---\nid: 7\nuid: abc123\ntype: task\ntitle: \"Enviar proposta\"\ntags:\n  - \"trabalho\"\ncreated: 2026-09-01T10:00:00-03:00\nupdated: 2026-09-02T10:00:00-03:00\nsource: \"telegram\"\nstatus: done\ndue: 2026-09-30T00:00:00-03:00\nsummary: \"Resumo\"\n---\n\n# Enviar proposta\n\n- [x] Enviar proposta\n\nCorpo da tarefa\n\n## Conexões\n- **derived_from:** [[Reunião]]\n- **← mentions:** [[Outra]]\n"
	it := newTestParser().markdownItem(FormatMarkdown, "Tarefas/Enviar proposta.md", doc, time.Time{})
	if it.Type != database.TypeTask || it.Status != "done" || it.DueAt == nil || it.Summary != "Resumo" || it.Ref != "uid:abc123" {
		t.Fatalf("round trip fields: %+v", it)
	}
	if it.Content != "Corpo da tarefa" {
		t.Fatalf("export sections not stripped: %q", it.Content)
	}
	if len(it.Links) != 1 || it.Links[0] != (Link{Target: "Reunião", Relation: "derived_from"}) || it.Meta["original_source"] != "telegram" {
		t.Fatalf("links/meta: %+v %+v", it.Links, it.Meta)
	}
}

func TestEvernote(t *testing.T) {
	enex := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE en-export SYSTEM "http://xml.evernote.com/pub/evernote-export4.dtd">
<en-export>
<note><title>Lista de compras</title>
<content><![CDATA[<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE en-note SYSTEM "http://xml.evernote.com/pub/enml2.dtd"><en-note><div><en-todo checked="true"/>Leite</div><div><en-todo/>Pão</div><en-media type="image/png" hash="x"/></en-note>]]></content>
<created>20230115T103000Z</created><tag>casa</tag><tag>mercado</tag>
<note-attributes><source-url>https://exemplo.com</source-url></note-attributes>
<resource><data encoding="base64">AAAA</data><mime>image/png</mime><resource-attributes><file-name>foto.png</file-name></resource-attributes></resource>
</note>
<note><title>Vazia</title><content><![CDATA[<en-note></en-note>]]></content></note>
</en-export>`
	items, err := newTestParser().parseENEX(strings.NewReader(enex), "Pessoal.enex")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 notes, got %d", len(items))
	}
	it := items[0]
	if it.Title != "Lista de compras" || !strings.Contains(it.Content, "- [x] Leite") || !strings.Contains(it.Content, "- [ ] Pão") {
		t.Fatalf("enml: %q", it.Content)
	}
	if !has(it.Tags, "mercado") || it.URL != "https://exemplo.com" || it.CreatedAt.Year() != 2023 || it.Meta["notebook"] != "Pessoal" {
		t.Fatalf("fields: %+v", it)
	}
}

func TestKeep(t *testing.T) {
	data := `{"color":"DEFAULT","isTrashed":false,"isPinned":true,"isArchived":false,"title":"Mercado","userEditedTimestampUsec":1700000000000000,"createdTimestampUsec":1690000000000000,
"listContent":[{"text":"Café","isChecked":false},{"text":"Açúcar","isChecked":true}],"labels":[{"name":"Casa"}]}`
	items, err := newTestParser().parseKeep([]byte(data), "Takeout/Keep/Mercado.json")
	if err != nil || len(items) != 1 {
		t.Fatalf("keep: %v %d", err, len(items))
	}
	it := items[0]
	if it.Content != "- [ ] Café\n- [x] Açúcar" || !has(it.Tags, "Casa") || it.Meta["pinned"] != true || it.CreatedAt.Year() != 2023 || it.Ref != "Mercado" {
		t.Fatalf("keep item: %+v", it)
	}
	trashed, _ := newTestParser().parseKeep([]byte(`{"isTrashed":true,"title":"x","textContent":"y"}`), "t.json")
	if len(trashed) != 0 {
		t.Fatal("trashed note imported")
	}
}

func TestBookmarks(t *testing.T) {
	doc := `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE><H1>Bookmarks</H1>
<DL><p>
  <DT><H3 PERSONAL_TOOLBAR_FOLDER="true">Barra de favoritos</H3>
  <DL><p>
    <DT><H3>Go</H3>
    <DL><p>
      <DT><A HREF="https://go.dev/doc/" ADD_DATE="1700000000" TAGS="golang,docs">Documentação Go</A>
      <DD>Referência oficial
    </DL><p>
    <DT><A HREF="https://exemplo.com/">Exemplo</A>
    <DT><A HREF="javascript:alert(1)">bookmarklet</A>
  </DL><p>
</DL>`
	items := newTestParser().parseBookmarks(doc)
	if len(items) != 2 {
		t.Fatalf("want 2 bookmarks, got %+v", items)
	}
	g := items[0]
	if g.Title != "Documentação Go" || g.Type != database.TypeArticle || !has(g.Tags, "Go") || !has(g.Tags, "golang") || g.Meta["folder"] != "Go" {
		t.Fatalf("bookmark: %+v", g)
	}
	if !strings.HasPrefix(g.Content, "Referência oficial") || !strings.HasSuffix(g.Content, "Fonte: https://go.dev/doc/") || g.CreatedAt.Year() != 2023 {
		t.Fatalf("bookmark content/date: %q %v", g.Content, g.CreatedAt)
	}
	if items[1].Meta["folder"] != nil || has(items[1].Tags, "Go") {
		t.Fatalf("folder scope leaked: %+v", items[1])
	}
}

func TestCSV(t *testing.T) {
	data := "\ufeffTítulo;Descrição;Etiquetas;Prazo;Concluído;Prioridade\nComprar tinta;Cor gelo;casa, obra;30/10/2026;não;alta\nPagar IPTU;;finanças;2026-11-05;sim;\n"
	items, err := newTestParser().parseCSV([]byte(data), "tarefas.csv", false)
	if err != nil || len(items) != 2 {
		t.Fatalf("csv: %v %+v", err, items)
	}
	a := items[0]
	if a.Type != database.TypeTask || a.Status != database.StatusOpen || a.DueAt == nil || a.DueAt.Day() != 30 || !has(a.Tags, "obra") {
		t.Fatalf("row 1: %+v", a)
	}
	if !strings.Contains(a.Content, "**Prioridade:** alta") || items[1].Status != database.StatusDone {
		t.Fatalf("extra columns/status: %q %+v", a.Content, items[1])
	}

	pocket := "title,url,time_added,tags,status\nArtigo,https://ex.com/a,1700000000,ia|go,unread\n"
	pi, _ := newTestParser().parseCSV([]byte(pocket), "pocket.csv", false)
	if len(pi) != 1 || pi[0].Type != database.TypeArticle || pi[0].Ref != "https://ex.com/a" || !has(pi[0].Tags, "ia") || pi[0].Meta["status"] != "unread" {
		t.Fatalf("pocket: %+v", pi)
	}

	todoist := "TYPE,CONTENT,DESCRIPTION,PRIORITY,INDENT,AUTHOR,RESPONSIBLE,DATE,DATE_LANG,TIMEZONE\nsection,Semana,,,,,,,,\ntask,Revisar contrato @trabalho,Cláusula 3,4,1,,,2026-10-01,pt,\nnote,Pedir ao jurídico,,,,,,,,\n"
	ti, _ := newTestParser().parseCSV([]byte(todoist), "Projeto X.csv", false)
	if len(ti) != 1 || ti[0].Format != FormatTodoist || ti[0].Title != "Revisar contrato" || !has(ti[0].Tags, "trabalho") || !has(ti[0].Tags, "Semana") ||
		ti[0].DueAt == nil || !strings.Contains(ti[0].Content, "Pedir ao jurídico") {
		t.Fatalf("todoist: %+v", ti)
	}

	notion := "Name,Status,Created\nPágina A,Em andamento,\"January 2, 2024 3:04 PM\"\n"
	ni, _ := newTestParser().parseCSV([]byte(notion), "db.csv", false)
	if len(ni) != 1 || ni[0].Title != "Página A" || ni[0].CreatedAt.Year() != 2024 {
		t.Fatalf("notion db: %+v", ni)
	}
	strict, _ := newTestParser().parseCSV([]byte(notion), "db.csv", true)
	if len(strict) != 0 {
		t.Fatal("strict mode should drop rows without content")
	}
}

func TestJSON(t *testing.T) {
	api := `{"nodes":[{"id":5,"uid":"u5","type":"task","title":"Renovar CNH","content":"Detran","tags":["docs"],"status":"done","due_at":"2026-10-30T00:00:00Z","created_at":"2026-01-02T10:00:00Z","source":"telegram","meta":{"x":1},"score":0.5}]}`
	items, err := newTestParser().parseJSON([]byte(api), "export.json", false)
	if err != nil || len(items) != 1 {
		t.Fatalf("json: %v %+v", err, items)
	}
	it := items[0]
	if it.Ref != "uid:u5" || it.Type != database.TypeTask || it.Status != database.StatusDone || it.DueAt == nil || !has(it.Tags, "docs") ||
		it.Meta["original_source"] != "telegram" || it.Meta["x"] == nil || it.CreatedAt.Year() != 2026 {
		t.Fatalf("api node: %+v", it)
	}
	lines := "{\"title\":\"A\",\"text\":\"um\"}\n{\"title\":\"B\",\"text\":\"dois\"}\n"
	jl, err := newTestParser().parseJSON([]byte(lines), "x.jsonl", false)
	if err != nil || len(jl) != 2 || jl[1].Content != "dois" {
		t.Fatalf("jsonl: %v %+v", err, jl)
	}
}

func TestKindle(t *testing.T) {
	doc := "\ufeffO Programador Pragmático (Andrew Hunt)\r\n- Seu destaque na página 12 | posição 170-172 | Adicionado: segunda-feira, 1 de janeiro de 2024 10:00:00\r\n\r\nCuide do seu ofício.\r\n==========\r\n" +
		"O Programador Pragmático (Andrew Hunt)\r\n- Seu destaque na página 12 | posição 170-172 | Adicionado: segunda-feira, 1 de janeiro de 2024 10:00:00\r\n\r\nCuide do seu ofício.\r\n==========\r\n" +
		"Deep Work (Cal Newport)\r\n- Your Note on page 3 | Location 40 | Added on Tuesday, February 6, 2024 9:15:00 PM\r\n\r\nRevisar\r\n==========\r\n" +
		"Deep Work (Cal Newport)\r\n- Your Bookmark on Location 50 | Added on Tuesday, February 6, 2024 9:20:00 PM\r\n\r\n\r\n==========\r\n"
	items := newTestParser().parseKindle(doc)
	if len(items) != 2 {
		t.Fatalf("want 2 books, got %+v", items)
	}
	pp := items[0]
	if pp.Title != "O Programador Pragmático" || pp.Meta["author"] != "Andrew Hunt" || pp.Meta["highlights"] != 1 || pp.CreatedAt.Year() != 2024 {
		t.Fatalf("book: %+v", pp)
	}
	if !strings.Contains(pp.Content, "> Cuide do seu ofício.") || !strings.Contains(pp.Content, "pos. 170-172") {
		t.Fatalf("highlight: %q", pp.Content)
	}
	if !strings.Contains(items[1].Content, "📝 Revisar") || items[1].CreatedAt.Month() != time.February {
		t.Fatalf("note: %+v", items[1])
	}
}

func TestVCard(t *testing.T) {
	doc := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Maria Souza\r\nN:Souza;Maria;;;\r\nitem1.EMAIL;TYPE=INTERNET:maria@exemplo.com\r\nTEL;TYPE=CELL:+55 11 99999-0000\r\nORG:ACME;Financeiro\r\nTITLE:Gerente\r\nBDAY:1990-01-15\r\nNOTE:Conheci no evento\\, em SP.\\nGosta de café.\r\nCATEGORIES:myContacts,Trabalho\r\nUID:abc-1\r\nEND:VCARD\r\n" +
		"BEGIN:VCARD\r\nVERSION:2.1\r\nN;CHARSET=UTF-8;ENCODING=QUOTED-PRINTABLE:Ara=C3=BAjo;Jo=C3=A3o;;;\r\nTEL;CELL:123\r\nNOTE;ENCODING=QUOTED-PRINTABLE:Linha longa que =\r\ncontinua\r\nEND:VCARD\r\n"
	items := newTestParser().parseVCard(doc)
	if len(items) != 2 {
		t.Fatalf("want 2 contacts: %+v", items)
	}
	m := items[0]
	if m.Type != database.TypePerson || m.Title != "Maria Souza" || m.Ref != "abc-1" || !has(m.Tags, "Trabalho") || has(m.Tags, "myContacts") {
		t.Fatalf("contact: %+v", m)
	}
	for _, want := range []string{"**E-mail:** maria@exemplo.com", "**Organização:** ACME / Financeiro", "**Aniversário:** 15/01/1990", "Conheci no evento, em SP.\nGosta de café."} {
		if !strings.Contains(m.Content, want) {
			t.Fatalf("content missing %q: %q", want, m.Content)
		}
	}
	j := items[1]
	if j.Title != "João Araújo" || !strings.Contains(j.Content, "Linha longa que continua") || !strings.Contains(j.Content, "123 (cell)") {
		t.Fatalf("vcard 2.1 QP: %+v", j)
	}
}

func TestICal(t *testing.T) {
	doc := "BEGIN:VCALENDAR\r\nX-WR-CALNAME:Trabalho\r\nBEGIN:VTIMEZONE\r\nTZID:America/Sao_Paulo\r\nEND:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:ev1@google.com\r\nDTSTART;TZID=America/Sao_Paulo:20261005T140000\r\nDTEND;TZID=America/Sao_Paulo:20261005T150000\r\nSUMMARY:Reunião\\, orçamento\r\nLOCATION:Sala 2\r\n" +
		"DESCRIPTION:Pauta:\\n- custos\r\nATTENDEE;CN=\"Maria Souza\";ROLE=REQ-PARTICIPANT:mailto:maria@exemplo.com\r\nRRULE:FREQ=WEEKLY\r\nBEGIN:VALARM\r\nDESCRIPTION:alarme\r\nEND:VALARM\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:ev2\r\nDTSTART;VALUE=DATE:20261012\r\nSUMMARY:Feriado\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:ev3\r\nDTSTART:20261013T120000Z\r\nSUMMARY:Cancelado\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n" +
		"BEGIN:VTODO\r\nUID:t1\r\nSUMMARY:Enviar relatório\r\nDUE;VALUE=DATE:20261020\r\nSTATUS:COMPLETED\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	items := newTestParser().parseICal(doc)
	if len(items) != 3 {
		t.Fatalf("want 3 items: %+v", items)
	}
	ev := items[0]
	if ev.Type != database.TypeEvent || ev.Title != "Reunião, orçamento" || ev.DueAt == nil || ev.DueAt.UTC().Hour() != 17 || ev.Ref != "ev1@google.com" {
		t.Fatalf("event: %+v", ev)
	}
	for _, want := range []string{"**Início:** 05/10/2026 14:00", "**Local:** Sala 2", "[[Maria Souza]]", "FREQ=WEEKLY", "- custos"} {
		if !strings.Contains(ev.Content, want) {
			t.Fatalf("event content missing %q: %q", want, ev.Content)
		}
	}
	if strings.Contains(ev.Content, "alarme") || ev.Meta["calendar"] != "Trabalho" {
		t.Fatalf("alarm leaked or calendar missing: %+v", ev)
	}
	if items[1].Meta["all_day"] != true || !strings.Contains(items[1].Content, "12/10/2026") {
		t.Fatalf("all-day: %+v", items[1])
	}
	if items[2].Type != database.TypeTask || items[2].Status != database.StatusDone || items[2].DueAt == nil {
		t.Fatalf("todo: %+v", items[2])
	}
}

func TestOPML(t *testing.T) {
	doc := `<?xml version="1.0"?><opml version="2.0"><head><title>Meus feeds</title></head><body>
<outline text="Tech"><outline type="rss" text="Go Blog" xmlUrl="https://go.dev/blog/feed.atom"/></outline>
<outline type="rss" text="HN" xmlUrl="https://news.ycombinator.com/rss"/>
<outline text="Projeto"><outline text="Fase 1" _note="detalhes"><outline text="Tarefa A"/></outline></outline>
<outline text="Solto"/>
</body></opml>`
	items, feeds, err := newTestParser().parseOPML([]byte(doc), "feeds.opml")
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 2 || feeds[0] != "https://go.dev/blog/feed.atom" {
		t.Fatalf("feeds: %v", feeds)
	}
	if len(items) != 2 || items[0].Title != "Projeto" || !strings.Contains(items[0].Content, "  - Tarefa A") || items[1].Title != "Meus feeds" {
		t.Fatalf("outlines: %+v", items)
	}
}

func TestParseDate(t *testing.T) {
	for in, want := range map[string]string{
		"2024-01-02":                "2024-01-02",
		"02/01/2024":                "2024-01-02",
		"1704164400":                "2024-01-02",
		"1704164400000":             "2024-01-02",
		"January 2, 2024":           "2024-01-02",
		"2024-01-02T10:00:00Z":      "2024-01-02",
		"2024-01-02 10:00:00+00:00": "2024-01-02",
	} {
		got, ok := parseDate(in, time.UTC)
		if !ok || got.UTC().Format("2006-01-02") != want {
			t.Errorf("parseDate(%q) = %v %v", in, got, ok)
		}
	}
	if _, ok := parseDate("every monday", time.UTC); ok {
		t.Error("natural language should not parse")
	}
}
