package web

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/database"
)

// sourceInfo maps node sources to a friendly name and icon for the overview.
var sourceInfo = map[string][2]string{
	"telegram": {"Telegram", "💬"}, "voice": {"Voz", "🎙️"}, "web": {"Web", "🖥️"}, "api": {"API", "🔌"}, "clip": {"Web Clipper", "✂️"},
	"watcher": {"Pasta inbox", "📂"}, "gmail": {"Gmail", "✉️"}, "newsletter": {"Newsletters", "📰"}, "calendar": {"Agenda", "📅"},
	"drive": {"Drive", "📁"}, "contacts": {"Contatos", "👥"}, "gtasks": {"Google Tasks", "✅"}, "youtube": {"YouTube", "▶️"},
	"rss": {"RSS", "📡"}, "zepp": {"Zepp", "❤️"}, "webhook": {"Saúde (webhook)", "🩺"}, "agent": {"IA", "🤖"},
	"routine": {"Rotinas", "⏰"}, "memory": {"Memória", "🧠"}, "import": {"Importação", "📥"}, "import:takeout": {"Google Takeout", "📦"}, "": {"Outros", "•"},
}

// sourceKey groups raw sources shown as one item (every "import:*" but Takeout).
func sourceKey(src string) string {
	if strings.HasPrefix(src, "import:") && src != "import:takeout" {
		return "import"
	}
	return src
}

// systemTags are added by integrations; they repeat the Sources ring, so Topics skip them.
var systemTags = map[string]bool{
	"email": true, "newsletter": true, "drive": true, "youtube": true, "curtido": true, "contato": true, "google-tasks": true,
	"keep": true, "clip": true, "imagem": true, "pdf": true, "documento": true, "voz": true, "briefing": true, "review": true,
	"diario": true, "semanal": true, "takeout": true, "atividade": true, "historico": true, "maps": true, "localizacao": true,
	"chrome": true, "navegacao": true, "google-play": true, "lugares": true, "pesquisas": true, "inscricoes": true,
	"playlists": true, "planilha": true, "apresentacao": true, "texto": true, "import": true, "memoria": true, "prioridades": true, "ia": true,
}

var jobLabels = map[string]string{
	"morning": "Briefing matinal", "evening": "Balanço noturno", "weekly": "Revisão semanal", "maintenance": "Manutenção",
	"backup": "Backup", "rss": "RSS", "gmail": "Gmail", "calendar": "Agenda", "drive": "Drive", "contacts": "Contatos",
	"google-tasks": "Google Tasks", "youtube": "YouTube", "takeout": "Takeout do Drive", "zepp": "Zepp",
	"update": "Atualizações", "models": "Catálogo de IA", "actions": "Tarefas de reuniões", "memory": "Memória", "cleanup": "Faxina semanal",
}

type overviewItem struct {
	Key     string         `json:"key"`
	Label   string         `json:"label"`
	Icon    string         `json:"icon,omitempty"`
	Color   string         `json:"color,omitempty"`
	Count   int            `json:"count"`
	Types   map[string]int `json:"types,omitempty"`
	Sources []string       `json:"sources,omitempty"` // raw sources grouped under Key
}

type overviewRoutine struct {
	Key     string    `json:"key"`
	Label   string    `json:"label"`
	Spec    string    `json:"spec"`
	Next    time.Time `json:"next"`
	Last    time.Time `json:"last,omitzero"`
	Error   string    `json:"error,omitempty"`
	Running bool      `json:"running,omitempty"`
}

// apiOverview summarises the brain for the orbit view: types, main topics, routines and sources.
func (s *Server) apiOverview(w http.ResponseWriter, r *http.Request) {
	var bySource, byTag map[string]map[string]int
	g, ctx := errgroup.WithContext(r.Context())
	g.Go(func() (err error) { bySource, err = s.DB.SourceTypeCounts(ctx); return })
	g.Go(func() (err error) { byTag, err = s.DB.TagTypeCounts(ctx); return })
	if err := g.Wait(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, s.overview(r.Context(), bySource, byTag))
}

func (s *Server) overview(ctx context.Context, bySource, byTag map[string]map[string]int) map[string]any {
	typeCount := map[string]int{}
	total := 0
	groups := map[string]*overviewItem{}
	for src, types := range bySource {
		key := sourceKey(src)
		it := groups[key]
		if it == nil {
			info, ok := sourceInfo[key]
			if !ok {
				info = [2]string{src, "•"}
			}
			it = &overviewItem{Key: key, Label: info[0], Icon: info[1], Types: map[string]int{}}
			groups[key] = it
		}
		it.Sources = append(it.Sources, src)
		for t, n := range types {
			it.Types[t] += n
			it.Count += n
			typeCount[t] += n
			total += n
		}
	}
	var sources []overviewItem
	for _, it := range groups {
		sort.Strings(it.Sources)
		sources = append(sources, *it)
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Count == sources[j].Count {
			return sources[i].Label < sources[j].Label
		}
		return sources[i].Count > sources[j].Count
	})
	if len(sources) > 16 { // keep the ring readable: fold the smallest into "Outros"
		other := overviewItem{Key: "_other", Label: "Outras fontes", Icon: "•", Types: map[string]int{}}
		for _, it := range sources[15:] {
			other.Count += it.Count
			other.Sources = append(other.Sources, it.Sources...)
			for t, n := range it.Types {
				other.Types[t] += n
			}
		}
		sources = append(sources[:15], other)
	}

	var types []overviewItem
	for _, t := range database.NodeTypes {
		types = append(types, overviewItem{Key: t, Label: typeLabels[t], Color: typeColors[t], Count: typeCount[t]})
	}

	var topics []overviewItem
	for tag, tt := range byTag {
		if systemTags[tag] {
			continue
		}
		it := overviewItem{Key: tag, Label: "#" + tag, Types: tt}
		for _, n := range tt {
			it.Count += n
		}
		if it.Count >= 2 {
			topics = append(topics, it)
		}
	}
	sort.Slice(topics, func(i, j int) bool {
		if topics[i].Count == topics[j].Count {
			return topics[i].Key < topics[j].Key
		}
		return topics[i].Count > topics[j].Count
	})
	topics = topics[:min(len(topics), 12)]
	for i := range topics { // colour of the type that dominates the topic
		best, bestType := 0, ""
		for t, n := range topics[i].Types {
			if n > best || (n == best && t < bestType) {
				best, bestType = n, t
			}
		}
		topics[i].Color = typeColors[bestType]
	}

	var routines []overviewRoutine
	if s.Hooks.Jobs != nil {
		for _, j := range s.Hooks.Jobs() {
			label := jobLabels[j.Name]
			if label == "" {
				label = j.Name
			}
			routines = append(routines, overviewRoutine{Key: j.Name, Label: label, Spec: j.Spec, Next: j.Next, Last: j.LastRun, Error: j.LastErr, Running: j.Running})
		}
		sort.Slice(routines, func(i, j int) bool { return routines[i].Key < routines[j].Key })
	}
	return map[string]any{
		"brain": s.Cfg.Get("BRAIN_NAME"), "total": total,
		"types": types, "topics": topics, "sources": sources, "routines": routines,
	}
}

type overviewList struct {
	Title string
	Color template.CSS
	Icon  string
	Count int
	Query string // params for the network view
	Nodes []database.Node
}

// overviewNodes lists the most recent nodes of one overview item (drill-down panel).
func (s *Server) overviewNodes(w http.ResponseWriter, r *http.Request) {
	f := s.filterFromQuery(r)
	f.Order, f.Limit = "", 40 // by content date, unknown dates last
	nodes, err := s.DB.ListNodes(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	q := r.URL.Query()
	v := overviewList{Title: q.Get("label"), Icon: q.Get("icon"), Nodes: nodes}
	v.Count, _ = strconv.Atoi(q.Get("count"))
	if len(f.Types) == 1 {
		v.Color = template.CSS(typeColors[f.Types[0]])
		if v.Title == "" {
			v.Title = typeLabels[f.Types[0]]
		}
	}
	params := url.Values{}
	for _, k := range []string{"types", "tag", "source"} {
		if val := q.Get(k); val != "" {
			params.Set(k, val)
		}
	}
	v.Query = params.Encode()
	s.fragment(w, "overview_nodes", v)
}
