package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
)

const webChannel = "web"

type chatView struct {
	Providers []llm.ProviderInfo
	History   []database.ChatMessage
	Enabled   bool
}

func (s *Server) chatPage(w http.ResponseWriter, r *http.Request) {
	hist, _ := s.DB.ChatHistory(r.Context(), webChannel, 60)
	s.render(w, "chat", s.page(r, "Chat", "chat", chatView{Providers: s.LLM.Providers(), History: hist, Enabled: s.LLM.Enabled()}))
}

func (s *Server) chatReset(w http.ResponseWriter, r *http.Request) {
	_ = s.DB.ClearChat(r.Context(), webChannel)
	redirectFlash(w, r, "/chat", "Nova conversa iniciada.", false)
}

// apiChat streams the assistant answer as Server-Sent Events.
func (s *Server) apiChat(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(20 << 20); err != nil && err != http.ErrNotMultipart {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	var mu sync.Mutex
	send := func(event string, data any) error {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	send("start", map[string]string{"provider": provider})
	res, err := s.Agent.Chat(r.Context(), agent.ChatRequest{Channel: webChannel, Provider: provider, Text: msg, Parts: parts},
		func(delta string) error { return send("token", map[string]string{"t": delta}) },
		func(ev agent.ChatEvent) { send(ev.Type, ev.Data) })
	if err != nil {
		s.log.Warn("chat falhou", "err", err)
		send("error", map[string]string{"message": err.Error()})
		return
	}
	send("done", map[string]any{"provider": res.Provider, "model": res.Model, "input_tokens": res.Usage.InputTokens, "output_tokens": res.Usage.OutputTokens})
}
