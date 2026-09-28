package takeout

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// ---- My Activity ----

type activity struct {
	Header    string   `json:"header"`
	Title     string   `json:"title"`
	TitleURL  string   `json:"titleUrl"`
	Time      string   `json:"time"`
	Products  []string `json:"products"`
	Subtitles []struct {
		Name string `json:"name"`
	} `json:"subtitles"`
	Details []struct {
		Name string `json:"name"`
	} `json:"details"`
}

// verbs prefixes My Activity titles ("Watched X", "Assistiu a X"); longest first.
var verbs = []string{
	"Searched for ", "Listened to ", "Watched ", "Searched ", "Visited ", "Viewed ", "Used ", "Liked ", "Saved ", "Shared ", "Installed ", "Opened ", "Played ", "Answered ",
	"Pesquisou por ", "Assistiu ao ", "Assistiu à ", "Assistiu a ", "Assistiu ", "Pesquisou ", "Visitou ", "Visualizou ", "Usou ", "Ouviu ", "Curtiu ", "Salvou ",
	"Compartilhou ", "Instalou ", "Abriu ", "Jogou ", "Respondeu ",
	"Buscaste ", "Viste ", "Visitaste ", "Has visto ", "Has buscado ",
}

func stripVerb(s string) string {
	s = strings.TrimSpace(s)
	for _, v := range verbs {
		if strings.HasPrefix(s, v) {
			return strings.TrimSpace(s[len(v):])
		}
	}
	return s
}

func domain(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(p.Hostname(), "www.")
}

func isSearchURL(u string) bool {
	return strings.Contains(u, "/search?") || strings.Contains(u, "/results?search_query") || strings.Contains(u, "search_query=")
}

func (b *batch) activity(raw json.RawMessage) {
	var a activity
	if json.Unmarshal(raw, &a) != nil {
		return
	}
	t, err := time.Parse(time.RFC3339, a.Time)
	if err != nil {
		return
	}
	for _, d := range a.Details { // ads shown on YouTube/Search are not the user's activity
		if strings.Contains(d.Name, "Google Ads") || strings.Contains(d.Name, "Anúncios") || strings.Contains(d.Name, "Anuncios") {
			return
		}
	}
	header := strings.TrimSpace(a.Header)
	if header == "" && len(a.Products) > 0 {
		header = a.Products[0]
	}
	if header == "" {
		header = "Google"
	}
	it := item{t: t, text: stripVerb(a.Title), url: a.TitleURL}
	if len(a.Subtitles) > 0 {
		it.extra = strings.TrimSpace(a.Subtitles[0].Name)
	}
	switch {
	case isSearchURL(a.TitleURL):
		it.key = strings.ToLower(it.text)
	case it.extra != "":
		it.key = it.extra
	default:
		it.key = firstNonEmpty(domain(a.TitleURL), it.text)
	}
	u := a.TitleURL
	if strings.Contains(strings.ToLower(header), "youtube") {
		switch {
		case strings.Contains(u, "/watch") || strings.Contains(u, "youtu.be/"):
			b.add("youtube-watch", "YouTube — vídeos assistidos", []string{"takeout", "youtube", "historico"}, it)
		case strings.Contains(u, "search_query"):
			b.add("youtube-search", "YouTube — pesquisas", []string{"takeout", "youtube", "pesquisas"}, it)
		default:
			b.add("youtube-other", "YouTube — outras atividades", []string{"takeout", "youtube"}, it)
		}
		return
	}
	s := slug(header)
	b.add("activity-"+s, header+" — atividade", []string{"takeout", "atividade", s}, it)
}

// ---- Chrome ----

func (b *batch) chrome(raw json.RawMessage) {
	var e struct {
		Title    string `json:"title"`
		URL      string `json:"url"`
		TimeUsec int64  `json:"time_usec"`
	}
	if json.Unmarshal(raw, &e) != nil || e.TimeUsec == 0 {
		return
	}
	if !strings.HasPrefix(e.URL, "http://") && !strings.HasPrefix(e.URL, "https://") {
		return
	}
	d := domain(e.URL)
	b.add("chrome", "Chrome — histórico de navegação", []string{"takeout", "chrome", "navegacao"},
		item{t: time.UnixMicro(e.TimeUsec), text: firstNonEmpty(e.Title, e.URL), url: e.URL, extra: d, key: d})
}

// ---- location history ----

var moveLabels = map[string]string{
	"WALKING": "🚶 a pé", "ON_FOOT": "🚶 a pé", "RUNNING": "🏃 correndo", "CYCLING": "🚲 bicicleta", "ON_BICYCLE": "🚲 bicicleta",
	"IN_PASSENGER_VEHICLE": "🚗 carro", "DRIVING": "🚗 carro", "IN_VEHICLE": "🚗 veículo", "IN_TAXI": "🚕 táxi", "MOTORCYCLING": "🏍️ moto",
	"IN_BUS": "🚌 ônibus", "IN_SUBWAY": "🚇 metrô", "IN_TRAIN": "🚆 trem", "IN_TRAM": "🚊 bonde", "FLYING": "✈️ avião",
	"IN_FERRY": "⛴️ balsa", "SAILING": "⛵ barco", "SKIING": "⛷️ esqui",
}

func moveLabel(t string) string {
	t = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(t), " ", "_"))
	if l, ok := moveLabels[t]; ok {
		return l
	}
	return "↔️ deslocamento"
}

func placeLabel(semantic string) string {
	switch strings.ToUpper(semantic) {
	case "HOME", "INFERRED_HOME", "TYPE_HOME":
		return "Casa"
	case "WORK", "INFERRED_WORK", "TYPE_WORK":
		return "Trabalho"
	case "SEARCHED_ADDRESS":
		return "Endereço pesquisado"
	case "ALIASED_LOCATION":
		return "Lugar marcado"
	}
	return "Lugar"
}

func mapsLink(lat, lng float64, placeID string) string {
	v := url.Values{"api": {"1"}, "query": {fmt.Sprintf("%.6f,%.6f", lat, lng)}}
	if placeID != "" {
		v.Set("query_place_id", placeID)
	}
	return "https://www.google.com/maps/search/?" + v.Encode()
}

func km(m float64) string {
	if m < 1000 {
		return fmt.Sprintf("%d m", int(math.Round(m)))
	}
	return strings.Replace(strconv.FormatFloat(m/1000, 'f', 1, 64), ".", ",", 1) + " km"
}

func until(end time.Time, loc *time.Location) string {
	if end.IsZero() {
		return ""
	}
	return " (até " + end.In(loc).Format("15:04") + ")"
}

type e7Location struct {
	Name         string `json:"name"`
	Address      string `json:"address"`
	PlaceID      string `json:"placeId"`
	LatE7        int64  `json:"latitudeE7"`
	LngE7        int64  `json:"longitudeE7"`
	SemanticType string `json:"semanticType"`
}

type duration struct {
	Start   string `json:"startTimestamp"`
	End     string `json:"endTimestamp"`
	StartMs string `json:"startTimestampMs"`
	EndMs   string `json:"endTimestampMs"`
}

func stamp(iso, ms string) time.Time {
	if t, err := time.Parse(time.RFC3339, iso); err == nil {
		return t
	}
	if n, err := strconv.ParseInt(ms, 10, 64); err == nil && n > 0 {
		return time.UnixMilli(n)
	}
	return time.Time{}
}

func (d duration) span() (time.Time, time.Time) {
	return stamp(d.Start, d.StartMs), stamp(d.End, d.EndMs)
}

// semantic handles the old Takeout "Semantic Location History" (with place names).
func (b *batch) semantic(raw json.RawMessage) {
	var o struct {
		PlaceVisit *struct {
			Location e7Location `json:"location"`
			Duration duration   `json:"duration"`
		} `json:"placeVisit"`
		ActivitySegment *struct {
			ActivityType string   `json:"activityType"`
			Distance     float64  `json:"distance"`
			Duration     duration `json:"duration"`
		} `json:"activitySegment"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return
	}
	tags := []string{"takeout", "maps", "localizacao"}
	const label = "Linha do tempo (Maps)"
	switch {
	case o.PlaceVisit != nil:
		l := o.PlaceVisit.Location
		start, end := o.PlaceVisit.Duration.span()
		name := firstNonEmpty(l.Name, l.Address, placeLabel(l.SemanticType))
		text := "📍 " + name
		if l.Address != "" && l.Address != name {
			text += " — " + strings.ReplaceAll(l.Address, "\n", ", ")
		}
		lat, lng := float64(l.LatE7)/1e7, float64(l.LngE7)/1e7
		b.add("location", label, tags, item{t: start, text: text + until(end, b.loc), url: mapsLink(lat, lng, l.PlaceID), key: name})
	case o.ActivitySegment != nil:
		a := o.ActivitySegment
		start, end := a.Duration.span()
		text := moveLabel(a.ActivityType)
		if a.Distance > 0 {
			text += " " + km(a.Distance)
		}
		b.add("location", label, tags, item{t: start, text: text + until(end, b.loc)})
	}
}

// flexFloat accepts numbers encoded as JSON numbers or strings (iOS exports).
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	if v, err := strconv.ParseFloat(strings.Trim(string(b), `"`), 64); err == nil {
		*f = flexFloat(v)
	}
	return nil
}

// latLng parses {"latLng":"-23.5°, -46.6°"} (Android) or "geo:-23.5,-46.6" (iOS).
func latLng(raw json.RawMessage) (float64, float64, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var o struct {
			LatLng string `json:"latLng"`
		}
		if json.Unmarshal(raw, &o) != nil {
			return 0, 0, false
		}
		s = o.LatLng
	}
	s = strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(s), "geo:"), "°", "")
	lat, lng, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, false
	}
	la, err1 := strconv.ParseFloat(strings.TrimSpace(lat), 64)
	lo, err2 := strconv.ParseFloat(strings.TrimSpace(lng), 64)
	return la, lo, err1 == nil && err2 == nil
}

// segment handles the Timeline export of the Maps app (Android "semanticSegments" and iOS arrays).
func (b *batch) segment(raw json.RawMessage) {
	var s struct {
		StartTime string `json:"startTime"`
		EndTime   string `json:"endTime"`
		Visit     *struct {
			TopCandidate struct {
				PlaceID       string          `json:"placeId"`
				SemanticType  string          `json:"semanticType"`
				PlaceLocation json.RawMessage `json:"placeLocation"`
			} `json:"topCandidate"`
		} `json:"visit"`
		Activity *struct {
			DistanceMeters flexFloat `json:"distanceMeters"`
			TopCandidate   struct {
				Type string `json:"type"`
			} `json:"topCandidate"`
		} `json:"activity"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	start, err := time.Parse(time.RFC3339, s.StartTime)
	if err != nil {
		return
	}
	end, _ := time.Parse(time.RFC3339, s.EndTime)
	tags := []string{"takeout", "maps", "localizacao"}
	const label = "Linha do tempo (Maps)"
	switch {
	case s.Visit != nil:
		c := s.Visit.TopCandidate
		name := placeLabel(c.SemanticType)
		it := item{t: start}
		if lat, lng, ok := latLng(c.PlaceLocation); ok {
			it.url = mapsLink(lat, lng, c.PlaceID)
			it.key = name
			if name == "Lugar" {
				it.key = fmt.Sprintf("%.3f,%.3f", round3(lat), round3(lng))
			}
			name += fmt.Sprintf(" (%.4f, %.4f)", lat, lng)
		}
		it.text = "📍 " + name + until(end, b.loc)
		b.add("location", label, tags, it)
	case s.Activity != nil:
		text := moveLabel(s.Activity.TopCandidate.Type)
		if d := float64(s.Activity.DistanceMeters); d > 0 {
			text += " " + km(d)
		}
		b.add("location", label, tags, item{t: start, text: text + until(end, b.loc)})
	}
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// ---- Maps saved places / reviews (GeoJSON) ----

func prop(m map[string]any, keys ...string) string {
	for _, k := range keys {
		for mk, v := range m {
			if strings.EqualFold(mk, k) {
				switch x := v.(type) {
				case string:
					if s := strings.TrimSpace(x); s != "" {
						return s
					}
				case float64:
					return strconv.FormatFloat(x, 'f', -1, 64)
				}
			}
		}
	}
	return ""
}

func sub(m map[string]any, key string) map[string]any {
	for mk, v := range m {
		if strings.EqualFold(mk, key) {
			if o, ok := v.(map[string]any); ok {
				return o
			}
		}
	}
	return map[string]any{}
}

func shortDate(s string) string {
	for _, l := range []string{time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02"} {
		if t, err := time.Parse(l, s); err == nil {
			return t.Format("02/01/2006")
		}
	}
	return s
}

func (b *batch) feature(file string, raw json.RawMessage) {
	var f struct {
		Geometry struct {
			Coordinates []float64 `json:"coordinates"`
		} `json:"geometry"`
		Properties map[string]any `json:"properties"`
	}
	if json.Unmarshal(raw, &f) != nil || f.Properties == nil {
		return
	}
	p := f.Properties
	loc := sub(p, "location")
	name := firstNonEmpty(prop(loc, "name", "business name"), prop(p, "title", "name"))
	addr := firstNonEmpty(prop(loc, "address"), prop(p, "address"))
	link := prop(p, "google_maps_url", "google maps url")
	if link == "" && len(f.Geometry.Coordinates) >= 2 && (f.Geometry.Coordinates[0] != 0 || f.Geometry.Coordinates[1] != 0) {
		link = mapsLink(f.Geometry.Coordinates[1], f.Geometry.Coordinates[0], "")
	}
	if name == "" && addr == "" && link == "" {
		return
	}
	line := "- **" + firstNonEmpty(name, addr, "(sem nome)") + "**"
	if addr != "" && addr != name {
		line += " — " + strings.ReplaceAll(addr, "\n", ", ")
	}
	if r := prop(p, "five_star_rating_published", "star_rating"); r != "" {
		line += " · ★" + r
	}
	if t := prop(p, "review_text_published", "comment", "note"); t != "" {
		line += " · " + strings.ReplaceAll(t, "\n", " ")
	}
	if link != "" {
		line += " · [mapa](" + link + ")"
	}
	if d := prop(p, "date", "published", "updated"); d != "" {
		line += " (" + shortDate(d) + ")"
	}
	list := strings.TrimSuffix(path.Base(file), path.Ext(file))
	b.list("maps:"+slug(list), "Google Maps — "+list, []string{"takeout", "maps", "lugares"}, line)
}

// ---- Google Play ----

var playKinds = map[string]string{
	"install": "apps instalados", "libraryDoc": "biblioteca", "purchaseHistory": "compras",
	"subscription": "assinaturas", "review": "avaliações", "orderHistory": "pedidos",
}

func (b *batch) play(raw json.RawMessage) {
	var m map[string]map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	for kind, v := range m {
		label, ok := playKinds[kind]
		if !ok {
			continue
		}
		doc := sub(v, "doc")
		if len(doc) == 0 {
			doc = sub(v, "document")
		}
		title := firstNonEmpty(prop(doc, "title"), prop(v, "title"))
		if title == "" {
			continue
		}
		line := "- **" + title + "**"
		if t := prop(doc, "documentType"); t != "" {
			line += " (" + t + ")"
		}
		var extra []string
		if d := prop(v, "firstInstallationTime", "acquisitionTime", "purchaseTime", "creationTime", "lastUpdateTime", "expirationDate"); d != "" {
			extra = append(extra, shortDate(d))
		}
		extra = append(extra, nonEmpty(prop(v, "invoicePrice"), prop(v, "state"), prop(sub(v, "deviceAttribute"), "deviceDisplayName", "model"))...)
		if r := prop(v, "starRating"); r != "" {
			extra = append(extra, "★"+r)
		}
		if c := prop(v, "comment"); c != "" {
			extra = append(extra, strings.ReplaceAll(c, "\n", " "))
		}
		if len(extra) > 0 {
			line += " — " + strings.Join(extra, " · ")
		}
		b.list("play:"+kind, "Google Play — "+label, []string{"takeout", "google-play"}, line)
	}
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
