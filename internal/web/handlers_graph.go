package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
)

type typeInfo struct {
	Type  string
	Label string
	Color string
	Count int
}

var typeLabels = map[string]string{
	database.TypeNote: "Notas", database.TypeTask: "Tarefas", database.TypePerson: "Pessoas", database.TypeEvent: "Eventos",
	database.TypeInsight: "Insights", database.TypeArticle: "Artigos", database.TypeHealth: "Saúde",
}

var typeColors = map[string]string{
	database.TypeNote: "#4f8cff", database.TypeTask: "#f5a524", database.TypePerson: "#e5484d", database.TypeEvent: "#30a46c",
	database.TypeInsight: "#8e4ec6", database.TypeArticle: "#12a594", database.TypeHealth: "#e93d82",
}

func (s *Server) graphPage(w http.ResponseWriter, r *http.Request) {
	counts, _ := s.DB.CountByType(r.Context())
	var types []typeInfo
	for _, t := range database.NodeTypes {
		types = append(types, typeInfo{Type: t, Label: typeLabels[t], Color: typeColors[t], Count: counts[t]})
	}
	s.render(w, "graph", s.page(r, "Mindmap", "graph", map[string]any{"Types": types, "Q": r.URL.Query().Get("q"), "Focus": r.URL.Query().Get("focus")}))
}

func (s *Server) filterFromQuery(r *http.Request) database.NodeFilter {
	q := r.URL.Query()
	f := database.NodeFilter{Tag: q.Get("tag"), Source: q.Get("source"), Status: q.Get("status")}
	for _, t := range strings.Split(q.Get("types"), ",") {
		if database.ValidType(t) {
			f.Types = append(f.Types, t)
		}
	}
	for _, t := range q["type"] {
		if database.ValidType(t) {
			f.Types = append(f.Types, t)
		}
	}
	loc := s.Cfg.Location()
	if d, err := time.ParseInLocation("2006-01-02", q.Get("from"), loc); err == nil {
		f.From = &d
	}
	if d, err := time.ParseInLocation("2006-01-02", q.Get("to"), loc); err == nil {
		end := d.AddDate(0, 0, 1)
		f.To = &end
	}
	return f
}

func (s *Server) apiGraph(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := s.filterFromQuery(r)
	limit, _ := strconv.Atoi(q.Get("limit"))
	gf := database.GraphFilter{NodeFilter: f, Limit: limit}
	var highlight []int64
	if text := strings.TrimSpace(q.Get("q")); text != "" {
		hits, err := s.Agent.Search(r.Context(), text, f, 40)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		for _, h := range hits {
			highlight = append(highlight, h.Node.ID)
		}
		gf.IDs, gf.Hops = highlight, 1
		if len(highlight) == 0 {
			writeJSON(w, 200, map[string]any{"nodes": []any{}, "edges": []any{}, "highlight": []int64{}})
			return
		}
	} else if id, err := strconv.ParseInt(q.Get("focus"), 10, 64); err == nil && id > 0 {
		gf.IDs, gf.Hops = []int64{id}, 1
		highlight = []int64{id}
	}
	g, err := s.DB.Subgraph(r.Context(), gf)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"nodes": g.Nodes, "edges": g.Edges, "highlight": highlight, "colors": typeColors})
}

type searchView struct {
	Q       string
	Results []agent.SearchResult
	Err     string
}

func (s *Server) searchPartial(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	res, err := s.Agent.Search(r.Context(), q, s.filterFromQuery(r), 30)
	v := searchView{Q: q, Results: res}
	if err != nil {
		v.Err = err.Error()
	}
	s.fragment(w, "search_results", v)
}

func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	res, err := s.Agent.Search(r.Context(), r.URL.Query().Get("q"), s.filterFromQuery(r), limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, res)
}

type nodeView struct {
	Node  *database.Node
	Links []database.Link
	Image string
	File  string
	Flash string
	Types []string
}

func (s *Server) loadNode(r *http.Request) (*database.Node, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, database.ErrNotFound
	}
	return s.DB.GetNode(r.Context(), id)
}

func (s *Server) renderNode(w http.ResponseWriter, r *http.Request, n *database.Node, flash string) {
	links, _ := s.DB.Neighbors(r.Context(), n.ID)
	v := nodeView{Node: n, Links: links, Flash: flash, Types: database.NodeTypes}
	if f, ok := n.Meta["file"].(string); ok && f != "" {
		ext := strings.ToLower(filepath.Ext(f))
		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp":
			v.Image = "/media/" + f
		default:
			v.File = "/media/" + f
		}
	}
	s.fragment(w, "node_detail", v)
}

func (s *Server) nodeDetail(w http.ResponseWriter, r *http.Request) {
	n, err := s.loadNode(r)
	if err != nil {
		http.Error(w, "nó não encontrado", http.StatusNotFound)
		return
	}
	s.renderNode(w, r, n, "")
}

func (s *Server) nodeEdit(w http.ResponseWriter, r *http.Request) {
	n, err := s.loadNode(r)
	if err != nil {
		http.Error(w, "nó não encontrado", http.StatusNotFound)
		return
	}
	s.fragment(w, "node_edit", nodeView{Node: n, Types: database.NodeTypes})
}

func (s *Server) parseDue(v string) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	loc := s.Cfg.Location()
	for _, l := range []string{"2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(l, v, loc); err == nil {
			return &t
		}
	}
	return nil
}

func splitTags(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' })
}

func triggerRefresh(w http.ResponseWriter, id int64) {
	b, _ := json.Marshal(map[string]any{"graph-refresh": map[string]int64{"id": id}})
	w.Header().Set("HX-Trigger", string(b))
}

func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	typ := r.FormValue("type")
	if !database.ValidType(typ) {
		typ = database.TypeNote
	}
	title, content := strings.TrimSpace(r.FormValue("title")), strings.TrimSpace(r.FormValue("content"))
	if title == "" && content == "" {
		http.Error(w, "informe título ou conteúdo", http.StatusBadRequest)
		return
	}
	n, _, err := s.Agent.Ingest(r.Context(), agent.IngestInput{
		Type: typ, Title: title, Content: content, Tags: splitTags(r.FormValue("tags")), DueAt: s.parseDue(r.FormValue("due")),
		Source: "web", Meta: map[string]any{"quick": title == ""}, Enrich: true,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	triggerRefresh(w, n.ID)
	s.renderNode(w, r, n, "Salvo. Auto-tagging e auto-linking em processamento…")
}

func (s *Server) nodeUpdate(w http.ResponseWriter, r *http.Request) {
	n, err := s.loadNode(r)
	if err != nil {
		http.Error(w, "nó não encontrado", http.StatusNotFound)
		return
	}
	if t := r.FormValue("type"); database.ValidType(t) {
		n.Type = t
	}
	n.Title = strings.TrimSpace(r.FormValue("title"))
	n.Content = r.FormValue("content")
	n.Summary = strings.TrimSpace(r.FormValue("summary"))
	n.Tags = splitTags(r.FormValue("tags"))
	n.DueAt = s.parseDue(r.FormValue("due"))
	if n.Type == database.TypeTask {
		if st := r.FormValue("status"); st == database.StatusDone || st == database.StatusOpen {
			n.Status = st
		} else if n.Status == "" {
			n.Status = database.StatusOpen
		}
	}
	if err := s.DB.UpdateNode(r.Context(), n); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = s.Agent.QueueEnrich(r.Context(), n.ID) // refresh embedding + links
	triggerRefresh(w, n.ID)
	s.renderNode(w, r, n, "Atualizado.")
}

func (s *Server) nodeDelete(w http.ResponseWriter, r *http.Request) {
	n, err := s.loadNode(r)
	if err != nil {
		http.Error(w, "nó não encontrado", http.StatusNotFound)
		return
	}
	if err := s.DB.DeleteNode(r.Context(), n.ID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if f, ok := n.Meta["file"].(string); ok && f != "" {
		os.Remove(filepath.Join(s.Agent.MediaDir(), filepath.Base(f)))
	}
	s.log.Info("nó removido", "id", n.ID, "title", n.Title)
	triggerRefresh(w, 0)
	fmt.Fprintf(w, `<div class="empty">Nó #%d removido.</div>`, n.ID)
}

func (s *Server) nodeToggle(w http.ResponseWriter, r *http.Request) {
	n, err := s.loadNode(r)
	if err != nil || n.Type != database.TypeTask {
		http.Error(w, "tarefa não encontrada", http.StatusNotFound)
		return
	}
	n.Status = database.StatusDone
	if r.FormValue("status") == database.StatusOpen {
		n.Status = database.StatusOpen
	}
	if err := s.DB.SetStatus(r.Context(), n.ID, n.Status); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	triggerRefresh(w, n.ID)
	s.renderNode(w, r, n, "")
}

func (s *Server) nodeEnrich(w http.ResponseWriter, r *http.Request) {
	n, err := s.loadNode(r)
	if err != nil {
		http.Error(w, "nó não encontrado", http.StatusNotFound)
		return
	}
	n.Meta["enriched"] = false
	delete(n.Meta, agent.MetaNoLLM)
	if err := s.DB.UpdateNode(r.Context(), n); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = s.Agent.QueueEnrich(r.Context(), n.ID)
	s.renderNode(w, r, n, "Reprocessamento enfileirado.")
}

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("file"))
	if name == "." || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
	http.ServeFile(w, r, filepath.Join(s.Agent.MediaDir(), name))
}

// ---- REST ----

func (s *Server) apiCreateNode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type    string   `json:"type"`
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
		Due     string   `json:"due"`
		Source  string   `json:"source"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !database.ValidType(in.Type) {
		in.Type = database.TypeNote
	}
	if in.Source == "" {
		in.Source = "api"
	}
	n, _, err := s.Agent.Ingest(r.Context(), agent.IngestInput{Type: in.Type, Title: in.Title, Content: in.Content, Tags: in.Tags, DueAt: s.parseDue(in.Due), Source: in.Source, Enrich: true})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, n)
}

func (s *Server) apiClip(w http.ResponseWriter, r *http.Request) {
	var in agent.ClipInput
	isJSON := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	if isJSON {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		in = agent.ClipInput{URL: r.FormValue("url"), Title: r.FormValue("title"), HTML: r.FormValue("html"), Text: r.FormValue("text"), Note: r.FormValue("note"), Tags: splitTags(r.FormValue("tags"))}
	}
	n, err := s.Agent.Clip(r.Context(), in)
	if isJSON {
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, map[string]any{"id": n.ID, "title": n.Title})
		return
	}
	p := Page{Title: "Web Clipper", Brain: s.Cfg.Get("BRAIN_NAME"), Data: map[string]any{"Node": n, "Err": errString(err), "Base": s.Cfg.PublicURL()}}
	if err != nil {
		w.WriteHeader(400)
	}
	s.render(w, "clip", p)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// apiHealthWebhook accepts {"date":"YYYY-MM-DD","metrics":{...},"source":"..."} or an array of them.
func (s *Server) apiHealthWebhook(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Date    string             `json:"date"`
		Metrics map[string]float64 `json:"metrics"`
		Source  string             `json:"source"`
	}
	body := http.MaxBytesReader(w, r.Body, 2<<20)
	var raw json.RawMessage
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var entries []entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		var one entry
		if err := json.Unmarshal(raw, &one); err != nil {
			writeJSON(w, 400, map[string]string{"error": "formato inválido"})
			return
		}
		entries = []entry{one}
	}
	stored := 0
	for _, e := range entries {
		if e.Source == "" {
			e.Source = "webhook"
		}
		if e.Date == "" {
			e.Date = time.Now().In(s.Cfg.Location()).Format("2006-01-02")
		}
		if err := s.Zepp.Store(r.Context(), e.Date, e.Metrics, e.Source); err != nil {
			status := 500
			if errors.Is(err, database.ErrNotFound) {
				status = 404
			}
			writeJSON(w, status, map[string]any{"error": err.Error(), "stored": stored})
			return
		}
		stored++
	}
	s.log.Info("métricas recebidas via webhook", "days", stored)
	writeJSON(w, 200, map[string]int{"stored": stored})
}
