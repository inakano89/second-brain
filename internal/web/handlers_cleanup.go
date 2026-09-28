package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
)

var cleanupHelp = map[string]string{
	database.CleanupDuplicate:     "Mesmo título e texto, ou o mesmo link. “Juntar” mantém o primeiro com as tags, conexões e o texto que só existir nas cópias.",
	database.CleanupNearDuplicate: "Textos quase iguais pela busca semântica. Confira antes: podem ser versões diferentes do mesmo assunto.",
	database.CleanupStaleTask:     "Tarefas abertas há mais de 30 dias sem mudança, ou com prazo vencido há mais de 30 dias.",
	database.CleanupEmpty:         "Notas, artigos e insights sem texto e sem conexões há mais de uma semana.",
	database.CleanupLonelyPerson:  "Pessoas que a IA criou a partir de uma única menção, sem dados. Apagar impede que a IA as crie de novo.",
}

type cleanupGroup struct {
	Kind, Label, Help string
	Items             []database.CleanupSuggestion
}

type cleanupView struct {
	Tab     string
	Cleanup int
	Trash   int
	Groups  []cleanupGroup
	Last    time.Time
}

func (s *Server) contentCleanup(w http.ResponseWriter, r *http.Request) {
	v := cleanupView{Tab: "cleanup"}
	var list []database.CleanupSuggestion
	g, gctx := errgroup.WithContext(r.Context())
	g.Go(func() (err error) { list, err = s.DB.ListCleanup(gctx, database.CleanupPending, 1000); return })
	g.Go(func() (err error) { v.Trash, err = s.DB.TrashCount(gctx); return })
	if err := g.Wait(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	byKind := map[string][]database.CleanupSuggestion{}
	for _, c := range list {
		byKind[c.Kind] = append(byKind[c.Kind], c)
	}
	for _, k := range agent.CleanupKinds {
		if items := byKind[k]; len(items) > 0 {
			v.Groups = append(v.Groups, cleanupGroup{Kind: k, Label: agent.CleanupLabels[k], Help: cleanupHelp[k], Items: items})
		}
	}
	v.Cleanup = len(list)
	if s.Hooks.Jobs != nil {
		for _, j := range s.Hooks.Jobs() {
			if j.Name == "cleanup" {
				v.Last = j.LastRun
			}
		}
	}
	s.render(w, "content", s.page(r, "Conteúdo · Faxina", "content", v))
}

// contentCleanupApply resolves one suggestion (merge, trash, done or dismiss).
func (s *Server) contentCleanupApply(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	msg, err := s.Agent.ApplyCleanup(r.Context(), id, r.FormValue("action"))
	if !isHTMX(r) {
		if err != nil {
			redirectFlash(w, r, "/content/cleanup", err.Error(), true)
			return
		}
		redirectFlash(w, r, "/content/cleanup", msg, false)
		return
	}
	data := map[string]any{"ID": id, "Message": msg}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.fragment(w, "cleanup_done", data)
}

func (s *Server) contentCleanupApplyAll(w http.ResponseWriter, r *http.Request) {
	n, err := s.Agent.ApplyAllCleanup(r.Context(), r.FormValue("kind"))
	if err != nil {
		redirectFlash(w, r, "/content/cleanup", fmt.Sprintf("%d aplicadas; parou em um erro: %v", n, err), true)
		return
	}
	redirectFlash(w, r, "/content/cleanup", fmt.Sprintf("%d sugestões aplicadas. O que foi apagado está na lixeira por 30 dias.", n), false)
}

// contentCleanupRun looks for suggestions now (same work as the weekly routine).
func (s *Server) contentCleanupRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	counts, err := s.Agent.SuggestCleanup(ctx)
	if err != nil {
		redirectFlash(w, r, "/content/cleanup", err.Error(), true)
		return
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	redirectFlash(w, r, "/content/cleanup", fmt.Sprintf("Busca concluída: %d sugestões.", total), false)
}
