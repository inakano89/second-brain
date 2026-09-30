package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
)

const (
	chatHistoryLimit = 100
	chatTitleMax     = 60
	chatInstructMax  = 2000
	chatAutoTitleMax = 32
	chatTurnTimeout  = 15 * time.Minute
	chatDefaultTitle = "Nova conversa"
)

// chatTab is a chat as the page and the JSON API show it.
type chatTab struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`   // what the tab says
	Persona string `json:"persona"` // preset key
	Name    string `json:"name"`    // persona name
	Icon    string `json:"icon"`
	Blurb   string `json:"blurb"`
	Custom  string `json:"custom,omitempty"` // custom/extra instructions
}

func tabOf(c database.Chat) chatTab {
	p := agent.PersonaFor(c.Persona, c.Instructions)
	t := chatTab{ID: c.ID, Title: c.Title, Persona: c.Persona, Name: p.Name, Icon: p.Icon, Blurb: p.Blurb, Custom: c.Instructions}
	if t.Title == "" {
		t.Title = p.Name
		if c.Persona == "" {
			t.Title = chatDefaultTitle
		}
	}
	return t
}

// chatPane is the messages of one chat (also the fragment loaded when a tab opens).
type chatPane struct {
	Tab     chatTab
	Greet   string
	History []database.ChatMessage
}

type chatView struct {
	Catalog []llm.ModelEntry
	Route   string
	Council llm.CouncilConfig
	Enabled bool
	Tabs    []chatTab
	Active  chatTab
	Pane    chatPane
	Groups  []agent.PersonaGroup
}

func (s *Server) chatPane(r *http.Request, c database.Chat) chatPane {
	hist, _ := s.Agent.ChatHistory(r.Context(), database.ChatChannel(c.ID), chatHistoryLimit)
	return chatPane{Tab: tabOf(c), Greet: agent.PersonaFor(c.Persona, c.Instructions).Greeting, History: hist}
}

// chatsOrDefault lists the tabs, opening a first (general) chat when there is none.
func (s *Server) chatsOrDefault(r *http.Request) ([]database.Chat, error) {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	chats, err := s.DB.ListChats(r.Context())
	if err != nil || len(chats) > 0 {
		return chats, err
	}
	c, err := s.DB.CreateChat(r.Context(), "", "", "")
	if err != nil {
		return nil, err
	}
	return []database.Chat{*c}, nil
}

func (s *Server) chatPage(w http.ResponseWriter, r *http.Request) {
	chats, err := s.chatsOrDefault(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	active := -1 // ?c=<id>; unknown or missing: the chat used last
	want, _ := strconv.ParseInt(r.URL.Query().Get("c"), 10, 64)
	for i, c := range chats {
		if c.ID == want {
			active = i
			break
		}
	}
	if active < 0 {
		active = 0
		for i, c := range chats {
			if c.UpdatedAt.After(chats[active].UpdatedAt) {
				active = i
			}
		}
	}
	v := chatView{Catalog: s.LLM.Catalog(), Route: s.LLM.Route(llm.TaskChat), Council: s.LLM.CouncilSetup(), Enabled: s.LLM.Enabled(),
		Groups: agent.PersonaGroups(), Pane: s.chatPane(r, chats[active])}
	v.Active = v.Pane.Tab
	for _, c := range chats {
		v.Tabs = append(v.Tabs, tabOf(c))
	}
	s.render(w, "chat", s.page(r, "Chat", "chat", v))
}

// chatFromPath loads the chat named in the URL or answers 404.
func (s *Server) chatFromPath(w http.ResponseWriter, r *http.Request) (*database.Chat, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, err := s.DB.GetChat(r.Context(), id)
	if err != nil {
		http.Error(w, "chat não encontrado", http.StatusNotFound)
		return nil, false
	}
	return c, true
}

// chatHistory returns the messages of a chat as an HTML fragment (opening a tab).
func (s *Server) chatHistory(w http.ResponseWriter, r *http.Request) {
	if c, ok := s.chatFromPath(w, r); ok {
		s.fragment(w, "chat_messages", s.chatPane(r, *c))
	}
}

// chatCreate opens a new chat tab, optionally with a persona.
func (s *Server) chatCreate(w http.ResponseWriter, r *http.Request) {
	persona := strings.TrimSpace(r.FormValue("persona"))
	instructions := extract.Truncate(strings.TrimSpace(r.FormValue("instructions")), chatInstructMax)
	title := extract.Truncate(strings.TrimSpace(r.FormValue("title")), chatTitleMax)
	switch {
	case !agent.ValidPersona(persona):
		http.Error(w, "persona desconhecida", http.StatusBadRequest)
		return
	case persona == agent.PersonaCustom && instructions == "":
		http.Error(w, "descreva quem a IA deve ser neste chat", http.StatusBadRequest)
		return
	}
	c, err := s.DB.CreateChat(r.Context(), title, persona, instructions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tabOf(*c))
}

// chatRename sets a chat's title; empty goes back to the automatic one.
func (s *Server) chatRename(w http.ResponseWriter, r *http.Request) {
	c, ok := s.chatFromPath(w, r)
	if !ok {
		return
	}
	title := extract.Truncate(strings.TrimSpace(r.FormValue("title")), chatTitleMax)
	if err := s.DB.RenameChat(r.Context(), c.ID, title); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c.Title = title
	writeJSON(w, http.StatusOK, tabOf(*c))
}

// chatClear wipes a chat's messages and keeps the tab (and its persona).
func (s *Server) chatClear(w http.ResponseWriter, r *http.Request) {
	c, ok := s.chatFromPath(w, r)
	if !ok {
		return
	}
	if err := s.DB.ClearChat(r.Context(), database.ChatChannel(c.ID)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// chatDelete removes a chat and its history.
func (s *Server) chatDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := s.chatFromPath(w, r)
	if !ok {
		return
	}
	if err := s.DB.DeleteChat(r.Context(), c.ID); err != nil && !errors.Is(err, database.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiChat streams the assistant answer as Server-Sent Events.
func (s *Server) apiChat(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(20 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("chat"), 10, 64)
	chat, err := s.DB.GetChat(r.Context(), id)
	if err != nil {
		http.Error(w, "chat não encontrado", http.StatusNotFound)
		return
	}
	msg := strings.TrimSpace(r.FormValue("message"))
	provider := r.FormValue("provider")
	var parts []llm.Part
	if r.MultipartForm != nil {
		for _, fh := range r.MultipartForm.File["file"] {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(io.LimitReader(f, 20<<20))
			f.Close()
			mime := agent.DetectMIME(fh.Filename, data)
			kind := llm.PartFile
			switch {
			case strings.HasPrefix(mime, "image/"):
				kind = llm.PartImage
			case strings.HasPrefix(mime, "audio/"):
				kind = llm.PartAudio
			case strings.HasPrefix(mime, "text/"):
				parts = append(parts, llm.Part{Type: llm.PartText, Text: fmt.Sprintf("Arquivo %s:\n%s", fh.Filename, string(data))})
				continue
			}
			parts = append(parts, llm.Part{Type: kind, MIME: mime, Data: data, Name: fh.Filename})
		}
	}
	if msg == "" && len(parts) == 0 {
		http.Error(w, "mensagem vazia", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming não suportado", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// The turn outlives the request: switching tab or page must not lose an answer that is
	// still being written, so the client leaving only stops the live stream.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), chatTurnTimeout)
	defer cancel()
	var (
		mu   sync.Mutex
		gone bool
	)
	send := func(event string, data any) error {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		if gone {
			return nil
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			gone = true
			return nil
		}
		flusher.Flush()
		return nil
	}
	send("start", map[string]string{"provider": provider})
	res, err := s.Agent.Chat(ctx, agent.ChatRequest{Channel: database.ChatChannel(chat.ID), Provider: provider, Text: msg, Parts: parts,
		Persona: chat.Persona, Instructions: chat.Instructions},
		func(delta string) error { return send("token", map[string]string{"t": delta}) },
		func(ev agent.ChatEvent) { send(ev.Type, ev.Data) })
	if err != nil {
		s.log.Warn("chat falhou", "chat", chat.ID, "err", err)
		send("error", map[string]string{"message": err.Error()})
		return
	}
	done := map[string]any{"provider": res.Provider, "model": res.Model, "input_tokens": res.Usage.InputTokens, "output_tokens": res.Usage.OutputTokens}
	_ = s.DB.TouchChat(ctx, chat.ID)
	if title := autoTitle(msg); title != "" {
		if ok, _ := s.DB.AutoTitleChat(ctx, chat.ID, title); ok {
			done["title"] = title
		}
	}
	send("done", done)
}

// autoTitle turns the first message of a chat into its tab title.
func autoTitle(msg string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	return extract.Truncate(strings.Join(strings.Fields(line), " "), chatAutoTitleMax)
}
