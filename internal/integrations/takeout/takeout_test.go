package takeout

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func fixture() map[string]string {
	usec := func(y int, m time.Month, d, h int) int64 { return time.Date(y, m, d, h, 0, 0, 0, time.UTC).UnixMicro() }
	ms := func(y int, m time.Month, d, h int) int64 { return time.Date(y, m, d, h, 0, 0, 0, time.UTC).UnixMilli() }
	return map[string]string{
		"Takeout/YouTube e YouTube Music/histórico/histórico-de-visualização.json": `[
 {"header":"YouTube","title":"Assistiu a Aula de Go","titleUrl":"https://www.youtube.com/watch?v=abc","subtitles":[{"name":"Canal Dev","url":"https://www.youtube.com/channel/x"}],"time":"2026-08-10T15:04:05.000Z","products":["YouTube"]},
 {"header":"YouTube","title":"Watched Receita de pão","titleUrl":"https://www.youtube.com/watch?v=def","subtitles":[{"name":"Cozinha"}],"time":"2026-09-01T12:00:00Z","products":["YouTube"]},
 {"header":"YouTube","title":"Watched Receita de pão","titleUrl":"https://www.youtube.com/watch?v=def","subtitles":[{"name":"Cozinha"}],"time":"2026-09-01T12:02:00Z","products":["YouTube"]},
 {"header":"YouTube","title":"Assistiu a Anúncio chato","titleUrl":"https://www.youtube.com/watch?v=ad","time":"2026-09-01T12:01:00Z","products":["YouTube"],"details":[{"name":"Dos Anúncios Google"}]},
 {"header":"YouTube","title":"Pesquisou golang generics","titleUrl":"https://www.youtube.com/results?search_query=golang+generics","time":"2026-09-02T10:00:00Z","products":["YouTube"]}]`,
		"Takeout/Minha atividade/Pesquisa/MinhaAtividade.json": `[{"header":"Pesquisa","title":"Pesquisou por preço do café","titleUrl":"https://www.google.com/search?q=pre%C3%A7o+do+caf%C3%A9","time":"2026-09-03T13:00:00Z","products":["Pesquisa"]}]`,
		"Takeout/Chrome/Histórico.json": fmt.Sprintf(`{"Browser History":[{"title":"Go Docs","url":"https://go.dev/doc","time_usec":%d,"page_transition":"LINK"},{"title":"Wiki","url":"https://pt.wikipedia.org/wiki/Go_(linguagem)","time_usec":%d},{"title":"cfg","url":"chrome://settings","time_usec":%d}]}`,
			usec(2026, 9, 6, 15), usec(2026, 9, 6, 15)+1, usec(2026, 9, 6, 16)),
		"Takeout/Histórico de localização/Semantic Location History/2026/2026_SETEMBRO.json": fmt.Sprintf(`{"timelineObjects":[
 {"placeVisit":{"location":{"name":"Padaria Central","address":"Rua A, 1","placeId":"P1","latitudeE7":-235000000,"longitudeE7":-466000000},"duration":{"startTimestamp":"2026-09-03T11:00:00Z","endTimestamp":"2026-09-03T11:30:00Z"}}},
 {"activitySegment":{"activityType":"WALKING","distance":1200,"duration":{"startTimestampMs":"%d","endTimestampMs":"%d"}}}]}`, ms(2026, 9, 3, 12), ms(2026, 9, 3, 13)),
		"Timeline.json": `{"semanticSegments":[
 {"startTime":"2026-09-04T08:00:00.000-03:00","endTime":"2026-09-04T17:00:00.000-03:00","visit":{"topCandidate":{"placeId":"W1","semanticType":"WORK","placeLocation":{"latLng":"-23.5611°, -46.6559°"}}}},
 {"startTime":"2026-09-04T17:00:00.000-03:00","endTime":"2026-09-04T17:40:00.000-03:00","activity":{"distanceMeters":8500.5,"topCandidate":{"type":"IN_PASSENGER_VEHICLE"}}}],
 "rawSignals":[{"position":{"LatLng":"1°, 2°","accuracyMeters":10}}],"userLocationProfile":{"frequentPlaces":[{"placeId":"W1"}]}}`,
		"location-history.json": `[{"startTime":"2026-09-05T09:00:00-03:00","endTime":"2026-09-05T10:00:00-03:00","visit":{"topCandidate":{"placeID":"H1","semanticType":"Home","placeLocation":"geo:-23.550000,-46.630000"}}},
 {"startTime":"2026-09-05T10:00:00-03:00","endTime":"2026-09-05T10:10:00-03:00","activity":{"distanceMeters":"350.0","topCandidate":{"type":"walking"}}}]`,
		"Takeout/Maps (seus lugares)/Lugares salvos.json": `{"type":"FeatureCollection","features":[{"geometry":{"coordinates":[-46.6,-23.5],"type":"Point"},"properties":{"date":"2024-05-01T10:00:00Z","google_maps_url":"http://maps.google.com/?cid=1","location":{"address":"Av. Paulista, 1000","name":"MASP","country_code":"BR"}},"type":"Feature"}]}`,
		"Takeout/Maps (seus lugares)/Avaliações.json":     `{"type":"FeatureCollection","features":[{"geometry":{"coordinates":[-46.7,-23.6],"type":"Point"},"properties":{"date":"2025-02-01T10:00:00Z","five_star_rating_published":5,"review_text_published":"Ótimo atendimento","google_maps_url":"http://maps.google.com/?cid=2","location":{"name":"Pizzaria Boa","address":"Rua B, 2"}},"type":"Feature"}]}`,
		"Takeout/Google Play Store/Installs.json":         `[{"install":{"doc":{"documentType":"Android Apps","title":"WhatsApp"},"firstInstallationTime":"2020-01-01T00:00:00.000Z","deviceAttribute":{"manufacturer":"samsung","deviceDisplayName":"Galaxy S21"}}}]`,
		"Takeout/Google Play Store/Purchase History.json": `[{"purchaseHistory":{"doc":{"documentType":"Subscription","title":"Premium"},"purchaseTime":"2025-01-01T00:00:00Z","invoicePrice":"R$ 9,90"}}]`,
	}
}

func TestRecognizes(t *testing.T) {
	for name, body := range fixture() {
		if !Recognizes([]byte(body)) {
			t.Errorf("%s not recognised", name)
		}
	}
	for _, body := range []string{
		`[{"title":"Nota","content":"x"}]`,
		`{"color":"DEFAULT","textContent":"keep","userEditedTimestampUsec":1}`,
		`{"name":{"formattedName":"Fulano"}}`,
		`[{"header":"x"}]`,
	} {
		if Recognizes([]byte(body)) {
			t.Errorf("%s wrongly recognised", body)
		}
	}
	if !Recognizes([]byte("\xef\xbb\xbf" + `{"Browser History":[]}`)) {
		t.Error("BOM not tolerated")
	}
	if !ActivityHTML("Takeout/YouTube/histórico/histórico-de-visualização.html") || ActivityHTML("Takeout/Keep/nota.html") {
		t.Error("ActivityHTML")
	}
}

func TestCollectorDigests(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	c := NewCollector(loc)
	for name, body := range fixture() {
		if err := c.Parse(name, strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	c.Parse("perfil.json", strings.NewReader(`{"name":{"formattedName":"Fulano"},"emails":[{"value":"f@x.com"}]}`))
	want := map[string]int{fmtActivity: 2, fmtChrome: 1, fmtSemantic: 1, fmtTimeline: 2, fmtPlaces: 2, fmtPlay: 2}
	got := c.Formats()
	for k, n := range want {
		if got[k] != n {
			t.Errorf("format %s = %d, want %d (all %v)", k, got[k], n, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected formats %v", got)
	}
	notes := map[string]Note{}
	for _, n := range c.Notes() {
		notes[n.Ref] = n
	}
	check := func(ref string, contains ...string) Note {
		t.Helper()
		n, ok := notes[ref]
		if !ok {
			t.Fatalf("note %s missing (have %d notes)", ref, len(notes))
		}
		for _, s := range contains {
			if !strings.Contains(n.Title+"\n"+n.Content, s) {
				t.Errorf("%s missing %q:\n%s\n%s", ref, s, n.Title, n.Content)
			}
		}
		return n
	}
	w := check("youtube-watch:2026-09", "YouTube — vídeos assistidos — setembro de 2026", "[Receita de pão](https://www.youtube.com/watch?v=def) — Cozinha", "1 registros", "Cozinha (1)")
	if strings.Contains(w.Content, "Anúncio") || w.Meta["enriched"] != true || w.CreatedAt.In(loc).Format("2006-01-02 15:04") != "2026-09-01 00:00" {
		t.Errorf("watch digest = %+v", w)
	}
	check("youtube-watch:2026-08", "Aula de Go", "Canal Dev")
	check("youtube-search:2026-09", "golang generics")
	check("activity-pesquisa:2026-09", "Pesquisa — atividade", "preço do café")
	ch := check("chrome:2026-09", "Go Docs", "go.dev (1)", "(https://pt.wikipedia.org/wiki/Go_%28linguagem%29)")
	if strings.Contains(ch.Content, "chrome://") {
		t.Error("internal chrome pages must be skipped")
	}
	check("location:2026-09", "📍 Padaria Central — Rua A, 1", "🚶 a pé 1,2 km", "📍 Trabalho (-23.5611, -46.6559)", "🚗 carro 8,5 km", "📍 Casa (-23.5500, -46.6300)", "🚶 a pé 350 m", "query_place_id=W1")
	check("maps:lugares-salvos", "Google Maps — Lugares salvos", "**MASP** — Av. Paulista, 1000", "(01/05/2024)")
	check("maps:avaliações", "★5", "Ótimo atendimento")
	check("play:install", "Google Play — apps instalados", "**WhatsApp** (Android Apps) — 01/01/2020 · Galaxy S21")
	check("play:purchaseHistory", "R$ 9,90")
	if c.Items() != 17 {
		t.Errorf("items = %d", c.Items())
	}
	c.WarnHTML("a.html")
	c.WarnHTML("b.html")
	if ws := c.Warnings(); len(ws) != 1 || !strings.Contains(ws[0], "JSON") {
		t.Errorf("warnings = %v", ws)
	}
}
