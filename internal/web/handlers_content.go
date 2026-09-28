package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/export"
)

const (
	contentPage   = 50
	bulkMax       = 20000 // "select every result" cap
	bulkEnrichMax = 1000  // LLM reprocessing costs money: keep one action bounded
)

var importLabels = map[string]string{
	"markdown": "Markdown/Obsidian", "notion": "Notion", "evernote": "Evernote", "keep": "Google Keep", "bookmarks": "Favoritos",
	"csv": "Planilha", "todoist": "Todoist", "json": "JSON", "kindle": "Kindle", "vcard": "Contatos (vCard)", "ical": "Agenda (.ics)",
	"opml": "OPML", "html": "Páginas HTML", "takeout": "Google Takeout",
}

// sourceLabel names a raw node source for people ("📥 Importação: Favoritos").
func sourceLabel(src string) string {
	if f, ok := strings.CutPrefix(src, "import:"); ok && src != "import:takeout" {
		name := importLabels[f]
		if name == "" {
			name = f
		}
		return "📥 Importação: " + name
	}
	if info, ok := sourceInfo[src]; ok {
		return info[1] + " " + info[0]
	}
	return "• " + src
}

var specialLabels = map[string]string{"dup": "Duplicados", "empty": "Sem conteúdo", "orphan": "Sem conexões"}

// contentQuery is the filter state of the Conteúdo page, as sent by the filter form.
type contentQuery struct {
	Q, Type, Source, Tag, Status, Special, Order, Batch, From, To string
	Offset                                                        int
}

func (s *Server) readContentQuery(get func(string) string) (contentQuery, database.NodeFilter) {
	q := contentQuery{
		Q: strings.TrimSpace(get("q")), Type: get("type"), Source: get("source"), Tag: strings.TrimPrefix(strings.TrimSpace(get("tag")), "#"),
		Status: get("status"), Special: get("special"), Order: get("order"), Batch: strings.TrimSpace(get("batch")),
		From: get("from"), To: get("to"),
	}
	q.Offset, _ = strconv.Atoi(get("offset"))
	q.Offset = max(q.Offset, 0)
	f := database.NodeFilter{Text: q.Q, Source: q.Source, Tag: q.Tag, Batch: q.Batch, Order: q.Order}
	if database.ValidType(q.Type) {
		f.Types = []string{q.Type}
	} else {
		q.Type = ""
	}
	if q.Status == database.StatusOpen || q.Status == database.StatusDone {
		f.Status = q.Status
	} else {
		q.Status = ""
	}
	if _, ok := specialLabels[q.Special]; ok {
		f.Special = q.Special
		if f.Order == "" && q.Special == "dup" {
			f.Order = "title" // duplicates side by side
		}
	} else {
		q.Special = ""
	}
	loc := s.Cfg.Location()
	if t, err := time.ParseInLocation("2006-01-02", q.From, loc); err == nil {
		f.From = &t
	} else {
		q.From = ""
	}
	if t, err := time.ParseInLocation("2006-01-02", q.To, loc); err == nil {
		end := t.AddDate(0, 0, 1)
		f.To = &end
	} else {
		q.To = ""
	}
	return q, f
}

// Values encodes the filter (offset only when set) for links and the address bar.
func (q contentQuery) Values(offset int) url.Values {
	v := url.Values{}
	for k, val := range map[string]string{"q": q.Q, "type": q.Type, "source": q.Source, "tag": q.Tag, "status": q.Status,
		"special": q.Special, "order": q.Order, "batch": q.Batch, "from": q.From, "to": q.To} {
		if val != "" {
			v.Set(k, val)
		}
	}
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	return v
}

// Active reports whether any filter is set.
func (q contentQuery) Active() bool { return len(q.Values(0)) > 0 }

type contentRows struct {
	Query      contentQuery
	Nodes      []database.Node
	Total      int
	First      int
	Last       int
	Prev, Next int
	HasPrev    bool
	HasNext    bool
	Flash      string
	Error      string
	Undo       string // trash batch of the last delete
	BulkMax    int
}

type contentOption struct {
	Value, Label string
	Count        int
}

type contentView struct {
	Tab        string
	Cleanup    int
	Rows       contentRows
	Types      []string
	Sources    []contentOption
	Specials   []contentOption
	Total      int
	Trash      int
	BatchLabel string
}

func (s *Server) contentRowsFor(ctx context.Context, q contentQuery, f database.NodeFilter) (contentRows, error) {
	rows := contentRows{Query: q, BulkMax: bulkMax}
	total, err := s.DB.CountNodes(ctx, f)
	if err != nil {
		return rows, err
	}
	if q.Offset >= total && total > 0 { // e.g. the last page was deleted
		q.Offset = (total - 1) / contentPage * contentPage
		rows.Query.Offset = q.Offset
	}
	f.Limit, f.Offset = contentPage, q.Offset
	if rows.Nodes, err = s.DB.ListNodes(ctx, f); err != nil {
		return rows, err
	}
	rows.Total = total
	if len(rows.Nodes) > 0 {
		rows.First, rows.Last = q.Offset+1, q.Offset+len(rows.Nodes)
	}
	rows.HasPrev, rows.Prev = q.Offset > 0, max(q.Offset-contentPage, 0)
	rows.HasNext, rows.Next = rows.Last < total, q.Offset+contentPage
	return rows, nil
}

func (s *Server) contentPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q, f := s.readContentQuery(r.URL.Query().Get)
	v := contentView{Tab: "items", Types: database.NodeTypes}
	var bySource map[string]map[string]int
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { v.Rows, err = s.contentRowsFor(gctx, q, f); return })
	g.Go(func() (err error) { bySource, err = s.DB.SourceTypeCounts(gctx); return })
	g.Go(func() (err error) { v.Trash, err = s.DB.TrashCount(gctx); return })
	g.Go(func() (err error) { v.Cleanup, err = s.DB.CleanupPendingCount(gctx); return })
	specials := []string{"dup", "empty", "orphan"}
	v.Specials = make([]contentOption, len(specials))
	for i, sp := range specials {
		g.Go(func() error {
			n, err := s.DB.CountNodes(gctx, database.NodeFilter{Special: sp})
			v.Specials[i] = contentOption{Value: sp, Label: specialLabels[sp], Count: n}
			return err
		})
	}
	if q.Batch != "" {
		g.Go(func() error {
			batches, err := s.DB.ImportBatches(gctx, 200)
			for _, b := range batches {
				if b.Batch == q.Batch {
					v.BatchLabel = b.Name
				}
			}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for src, types := range bySource {
		o := contentOption{Value: src, Label: sourceLabel(src)}
		for _, n := range types {
			o.Count += n
		}
		v.Total += o.Count
		v.Sources = append(v.Sources, o)
	}
	sort.Slice(v.Sources, func(i, j int) bool { return v.Sources[i].Label < v.Sources[j].Label })
	if v.BatchLabel == "" && q.Batch != "" {
		v.BatchLabel = "envio " + q.Batch
	}
	s.render(w, "content", s.page(r, "Conteúdo", "content", v))
}

// contentRowsPartial re-renders the list (filters, pagination, refresh after edits).
func (s *Server) contentRowsPartial(w http.ResponseWriter, r *http.Request) {
	q, f := s.readContentQuery(r.URL.Query().Get)
	s.writeRows(w, r, q, f, contentRows{})
}

func (s *Server) writeRows(w http.ResponseWriter, r *http.Request, q contentQuery, f database.NodeFilter, msg contentRows) {
	rows, err := s.contentRowsFor(r.Context(), q, f)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	rows.Flash, rows.Error, rows.Undo = msg.Flash, msg.Error, msg.Undo
	if r.Method == http.MethodGet && isHTMX(r) {
		u := "/content"
		if v := rows.Query.Values(rows.Query.Offset); len(v) > 0 {
			u += "?" + v.Encode()
		}
		w.Header().Set("HX-Push-Url", u)
	}
	s.fragment(w, "content_rows", rows)
}

// selectedIDs returns the ids of the selection, or of every filter result when all=1.
func (s *Server) selectedIDs(r *http.Request, f database.NodeFilter) ([]int64, error) {
	if r.FormValue("all") == "1" {
		return s.DB.NodeIDs(r.Context(), f, bulkMax)
	}
	var ids []int64
	for _, v := range r.Form["id"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return humanInt(int64(n)) + " " + many
}

// contentBulk applies one action to the selection and re-renders the list.
func (s *Server) contentBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	q, f := s.readContentQuery(r.FormValue)
	ids, err := s.selectedIDs(r, f)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var msg contentRows
	if len(ids) == 0 {
		msg.Error = "Nenhum item selecionado."
		s.writeRows(w, r, q, f, msg)
		return
	}
	update := func(fn func(n *database.Node) bool) int {
		n, uerr := s.DB.UpdateNodes(ctx, ids, fn)
		if uerr != nil {
			err = uerr
		}
		return n
	}
	switch action := r.FormValue("action"); action {
	case "trash":
		forget := r.FormValue("forget") != ""
		var n int
		msg.Undo, n, err = s.DB.TrashNodes(ctx, ids, forget)
		msg.Flash = plural(n, "item movido", "itens movidos") + " para a lixeira (30 dias)."
		if forget {
			msg.Flash += " Não voltam em sincronizações e importações."
		}
		s.log.Info("conteúdo apagado", "count", n, "forget", forget, "batch", msg.Undo)
	case "tag_add", "tag_remove":
		tags := database.NormalizeTags(strings.FieldsFunc(r.FormValue("set_tag"), func(r rune) bool { return r == ',' || r == ';' }))
		if len(tags) == 0 {
			msg.Error = "Informe a tag."
			break
		}
		n := update(func(n *database.Node) bool {
			before := len(n.Tags)
			if action == "tag_add" {
				n.Tags = database.NormalizeTags(append(n.Tags, tags...))
				return len(n.Tags) != before
			}
			n.Tags = slices.DeleteFunc(n.Tags, func(t string) bool { return slices.Contains(tags, t) })
			return len(n.Tags) != before
		})
		verb := "adicionada a"
		if action == "tag_remove" {
			verb = "removida de"
		}
		msg.Flash = fmt.Sprintf("Tag #%s %s %s.", strings.Join(tags, ", #"), verb, plural(n, "item", "itens"))
		if n == 0 {
			msg.Flash = "Nada mudou: os itens selecionados já estavam assim."
		}
	case "retype":
		typ := r.FormValue("to_type")
		if !database.ValidType(typ) {
			msg.Error = "Escolha o novo tipo."
			break
		}
		n := update(func(n *database.Node) bool {
			if n.Type == typ {
				return false
			}
			n.Type = typ
			switch {
			case typ == database.TypeTask && n.Status == "":
				n.Status = database.StatusOpen
			case typ != database.TypeTask:
				n.Status = ""
			}
			return true
		})
		msg.Flash = fmt.Sprintf("%s movido(s) para %s.", plural(n, "item", "itens"), typeLabels[typ])
		if n == 0 {
			msg.Flash = "Nada mudou: os itens selecionados já eram " + typeLabels[typ] + "."
		}
	case "done", "reopen":
		st := database.StatusDone
		if action == "reopen" {
			st = database.StatusOpen
		}
		n := update(func(n *database.Node) bool {
			if n.Type != database.TypeTask || n.Status == st {
				return false
			}
			n.Status = st
			return true
		})
		msg.Flash = plural(n, "tarefa atualizada", "tarefas atualizadas") + "."
		if n == 0 {
			msg.Flash = "Nada mudou: a seleção não tem tarefas nessa situação."
		}
	case "enrich":
		if len(ids) > bulkEnrichMax {
			ids = ids[:bulkEnrichMax]
		}
		n := update(func(n *database.Node) bool {
			n.Meta["enriched"] = false
			delete(n.Meta, agent.MetaNoLLM)
			return true
		})
		for _, id := range ids {
			if qerr := s.Agent.QueueEnrich(ctx, id); qerr != nil {
				err = qerr
				break
			}
		}
		msg.Flash = plural(n, "item enviado", "itens enviados") + " para reprocessamento (fila em segundo plano)."
		if n == bulkEnrichMax {
			msg.Flash += fmt.Sprintf(" Limite de %d por vez.", bulkEnrichMax)
		}
	default:
		msg.Error = "Ação desconhecida."
	}
	if err != nil {
		msg.Flash, msg.Error = "", err.Error()
	}
	s.writeRows(w, r, q, f, msg)
}

// contentUndo restores the last delete from the list.
func (s *Server) contentUndo(w http.ResponseWriter, r *http.Request) {
	q, f := s.readContentQuery(r.FormValue)
	var msg contentRows
	n, err := s.DB.RestoreBatch(r.Context(), r.FormValue("undo"))
	if err != nil {
		msg.Error = err.Error()
	} else {
		msg.Flash = plural(n, "item restaurado", "itens restaurados") + "."
	}
	s.writeRows(w, r, q, f, msg)
}

// contentExport downloads the selection as an Obsidian vault.
func (s *Server) contentExport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, f := s.readContentQuery(r.FormValue)
	ids, err := s.selectedIDs(r, f)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if len(ids) == 0 {
		http.Error(w, "nenhum item selecionado", http.StatusBadRequest)
		return
	}
	name := "second-brain-selecao-" + time.Now().Format("20060102-1504") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if err := export.ObsidianNodes(r.Context(), s.DB, w, s.Cfg.Location(), ids); err != nil {
		s.log.Error("export da seleção falhou", "err", err)
	}
}

// ---- Envios ----

type sendsView struct {
	Tab     string
	Cleanup int
	Batches []database.ImportBatch
	Sources []contentOption
	Trash   int
}

func (s *Server) contentSends(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := sendsView{Tab: "sends"}
	var bySource map[string]map[string]int
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { v.Batches, err = s.DB.ImportBatches(gctx, 200); return })
	g.Go(func() (err error) { bySource, err = s.DB.SourceTypeCounts(gctx); return })
	g.Go(func() (err error) { v.Trash, err = s.DB.TrashCount(gctx); return })
	g.Go(func() (err error) { v.Cleanup, err = s.DB.CleanupPendingCount(gctx); return })
	if err := g.Wait(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for src, types := range bySource {
		o := contentOption{Value: src, Label: sourceLabel(src)}
		for _, n := range types {
			o.Count += n
		}
		v.Sources = append(v.Sources, o)
	}
	sort.Slice(v.Sources, func(i, j int) bool {
		if v.Sources[i].Count == v.Sources[j].Count {
			return v.Sources[i].Label < v.Sources[j].Label
		}
		return v.Sources[i].Count > v.Sources[j].Count
	})
	s.render(w, "content", s.page(r, "Conteúdo · Envios", "content", v))
}

// contentSendTrash deletes a whole import batch or everything from one source.
func (s *Server) contentSendTrash(w http.ResponseWriter, r *http.Request) {
	f := database.NodeFilter{Batch: strings.TrimSpace(r.FormValue("batch")), Source: r.FormValue("source")}
	if f.Batch == "" && f.Source == "" {
		redirectFlash(w, r, "/content/sends", "Envio não informado.", true)
		return
	}
	ids, err := s.DB.NodeIDs(r.Context(), f, bulkMax)
	if err == nil {
		var n int
		_, n, err = s.DB.TrashNodes(r.Context(), ids, r.FormValue("forget") != "")
		if err == nil {
			s.log.Info("envio apagado", "batch", f.Batch, "source", f.Source, "count", n)
			redirectFlash(w, r, "/content/trash", plural(n, "item movido", "itens movidos")+" para a lixeira. Para desfazer, use “Restaurar” no lote abaixo.", false)
			return
		}
	}
	redirectFlash(w, r, "/content/sends", err.Error(), true)
}

// ---- Lixeira ----

type trashView struct {
	Tab      string
	Cleanup  int
	Trash    int
	Items    []database.TrashItem
	Batches  []database.TrashBatch
	Total    int
	Blocked  int
	Q        string
	Batch    string
	Offset   int
	Prev     int
	Next     int
	HasPrev  bool
	HasNext  bool
	Retainer int // days
}

func (s *Server) contentTrash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	qs := r.URL.Query()
	v := trashView{Tab: "trash", Q: strings.TrimSpace(qs.Get("q")), Batch: qs.Get("batch"), Retainer: int(database.TrashRetention.Hours() / 24)}
	v.Offset, _ = strconv.Atoi(qs.Get("offset"))
	v.Offset = max(v.Offset, 0)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		v.Items, v.Total, err = s.DB.ListTrash(gctx, v.Q, v.Batch, contentPage, v.Offset)
		return
	})
	g.Go(func() (err error) { v.Batches, err = s.DB.TrashBatches(gctx, 30); return })
	g.Go(func() (err error) { v.Blocked, err = s.DB.DeletedRefCount(gctx); return })
	g.Go(func() (err error) { v.Trash, err = s.DB.TrashCount(gctx); return })
	g.Go(func() (err error) { v.Cleanup, err = s.DB.CleanupPendingCount(gctx); return })
	if err := g.Wait(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	v.HasPrev, v.Prev = v.Offset > 0, max(v.Offset-contentPage, 0)
	v.HasNext, v.Next = v.Offset+len(v.Items) < v.Total, v.Offset+contentPage
	s.render(w, "content", s.page(r, "Conteúdo · Lixeira", "content", v))
}

func parseIDs(vals []string) []int64 {
	var ids []int64
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && id > 0 {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// contentTrashAction restores or purges trash items (selection, batch or everything).
func (s *Server) contentTrashAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	ids := parseIDs(r.Form["id"])
	batch := r.FormValue("batch")
	var (
		n    int
		err  error
		text string
	)
	switch r.FormValue("action") {
	case "restore":
		if batch != "" {
			n, err = s.DB.RestoreBatch(ctx, batch)
		} else {
			n, err = s.DB.RestoreTrash(ctx, ids)
		}
		text = plural(n, "item restaurado", "itens restaurados") + "."
	case "purge":
		if batch != "" {
			ids, err = s.DB.TrashBatchIDs(ctx, batch)
		}
		if err == nil && len(ids) > 0 {
			n, err = s.Agent.PurgeTrash(ctx, ids, time.Time{})
		}
		text = plural(n, "item apagado", "itens apagados") + " definitivamente."
	case "empty":
		n, err = s.Agent.PurgeTrash(ctx, nil, time.Now().Add(time.Minute))
		text = "Lixeira esvaziada (" + plural(n, "item", "itens") + ")."
	case "unblock":
		n, err = s.DB.ClearDeletedRefs(ctx)
		text = plural(n, "item apagado pode", "itens apagados podem") + " voltar a ser importado(s) nas próximas sincronizações."
	default:
		err = fmt.Errorf("ação desconhecida")
	}
	if err != nil {
		redirectFlash(w, r, "/content/trash", err.Error(), true)
		return
	}
	s.log.Info("lixeira", "action", r.FormValue("action"), "count", n)
	redirectFlash(w, r, "/content/trash", text, false)
}
