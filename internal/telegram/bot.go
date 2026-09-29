package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/crypto"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/profile"
	"github.com/inakano89/second-brain/internal/queue"
)

// Task kinds.
const (
	TaskMedia = "telegram.media"
	TaskSend  = "telegram.send"
)

// Service runs the bot and implements the scheduler Notifier.
type Service struct {
	cfg   *config.Config
	db    *database.DB
	agent *agent.Agent
	log   *slog.Logger

	chatsMu sync.Mutex
	chats   map[int64]chan *Message

	// Briefing is injected by main to serve /brief.
	Briefing func(ctx context.Context) (string, error)
}

// New creates the service.
func New(cfg *config.Config, db *database.DB, ag *agent.Agent, log *slog.Logger) *Service {
	return &Service{cfg: cfg, db: db, agent: ag, log: log.With("component", "telegram"), chats: map[int64]chan *Message{}}
}

func (s *Service) client() *Client {
	if t := s.cfg.Get("TELEGRAM_BOT_TOKEN"); t != "" {
		return NewClient(t)
	}
	return nil
}

// Enabled reports whether a bot token is configured.
func (s *Service) Enabled() bool { return s.cfg.Get("TELEGRAM_BOT_TOKEN") != "" }

// AllowedIDs parses ALLOWED_TELEGRAM_USER_IDS.
func (s *Service) AllowedIDs() []int64 {
	var out []int64
	for _, x := range s.cfg.GetList("ALLOWED_TELEGRAM_USER_IDS") {
		if id, err := strconv.ParseInt(x, 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}

func (s *Service) isAllowed(m *Message) bool {
	if m.From == nil {
		return false
	}
	for _, id := range s.AllowedIDs() {
		if id == m.From.ID {
			return true
		}
	}
	return false
}

func (s *Service) tmpDir() string {
	d := filepath.Join(s.cfg.GetPath("DATA_DIR"), "tmp")
	os.MkdirAll(d, 0o755)
	return d
}

// Run long-polls updates until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	c := s.client()
	if c == nil {
		return
	}
	me, err := c.GetMe(ctx)
	if err != nil {
		s.log.Error("token do Telegram inválido", "err", err)
		return
	}
	_ = c.DeleteWebhook(ctx)
	s.log.Info("bot Telegram conectado", "username", me.Username, "allowed", len(s.AllowedIDs()))
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		ups, err := c.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var ae *APIError
			if errors.As(err, &ae) && ae.RetryAfter > 0 {
				backoff = time.Duration(ae.RetryAfter) * time.Second
			}
			s.log.Warn("getUpdates falhou", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 2*time.Minute)
			continue
		}
		backoff = time.Second
		for _, u := range ups {
			offset = u.UpdateID + 1
			if u.Message == nil {
				continue
			}
			s.dispatch(ctx, c, u.Message)
		}
	}
}

// dispatch routes messages to one goroutine per chat, preserving order while
// different chats are processed concurrently. Idle chat workers exit.
func (s *Service) dispatch(ctx context.Context, c *Client, m *Message) {
	s.chatsMu.Lock()
	ch, ok := s.chats[m.Chat.ID]
	if !ok {
		ch = make(chan *Message, 32)
		s.chats[m.Chat.ID] = ch
		go s.chatWorker(ctx, c, m.Chat.ID, ch)
	}
	// Non-blocking send under the lock so an exiting worker cannot strand it.
	select {
	case ch <- m:
	default:
		s.log.Warn("fila do chat cheia, mensagem descartada", "chat_id", m.Chat.ID)
	}
	s.chatsMu.Unlock()
}

func (s *Service) chatWorker(ctx context.Context, c *Client, chatID int64, ch chan *Message) {
	idle := time.NewTimer(5 * time.Minute)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-ch:
			s.handle(ctx, c, m)
			idle.Reset(5 * time.Minute)
		case <-idle.C:
			s.chatsMu.Lock()
			if len(ch) > 0 {
				s.chatsMu.Unlock()
				idle.Reset(time.Minute)
				continue
			}
			delete(s.chats, chatID)
			s.chatsMu.Unlock()
			return
		}
	}
}

func (s *Service) handle(ctx context.Context, c *Client, m *Message) {
	if !s.isAllowed(m) {
		s.recordPending(ctx, c, m)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("panic no handler do Telegram", "panic", fmt.Sprint(r))
		}
	}()
	switch {
	case m.Voice != nil:
		s.enqueueMedia(ctx, c, m, m.Voice.FileID, "voice.ogg", firstNonEmpty(m.Voice.MimeType, "audio/ogg"), true)
	case m.Audio != nil:
		s.enqueueMedia(ctx, c, m, m.Audio.FileID, firstNonEmpty(m.Audio.FileName, "audio.mp3"), m.Audio.MimeType, true)
	case m.VideoNote != nil:
		c.SendMessage(ctx, m.Chat.ID, "Vídeo-mensagens não são suportadas; envie áudio.", m.MessageID)
	case len(m.Photo) > 0:
		best := m.Photo[len(m.Photo)-1]
		s.enqueueMedia(ctx, c, m, best.FileID, "photo.jpg", "image/jpeg", false)
	case m.Document != nil:
		if m.Document.FileSize > 20<<20 {
			c.SendMessage(ctx, m.Chat.ID, "Arquivo acima de 20 MB (limite da Bot API).", m.MessageID)
			return
		}
		s.enqueueMedia(ctx, c, m, m.Document.FileID, firstNonEmpty(m.Document.FileName, "arquivo"), m.Document.MimeType, false)
	case strings.HasPrefix(m.Text, "/"):
		s.command(ctx, c, m)
	case strings.TrimSpace(m.Text) != "":
		if s.diaryAnswer(ctx, c, m, m.Text) {
			return
		}
		s.converse(ctx, c, m, m.Text)
	}
}

// diaryAnswer takes a plain message as the answer to the pending diary question.
func (s *Service) diaryAnswer(ctx context.Context, c *Client, m *Message, text string) bool {
	if s.agent.DiaryActive(ctx) == nil {
		return false
	}
	reply, err := s.agent.DiaryAnswer(ctx, text)
	if err != nil {
		s.log.Warn("diário: resposta não salva", "err", err)
		return false
	}
	c.SendMessage(ctx, m.Chat.ID, reply, m.MessageID)
	return true
}

func firstNonEmpty(ss ...string) string {
	for _, x := range ss {
		if x != "" {
			return x
		}
	}
	return ""
}

type mediaPayload struct {
	ChatID    int64  `json:"chat_id"`
	MessageID int64  `json:"message_id"`
	Path      string `json:"path"`
	Filename  string `json:"filename"`
	MIME      string `json:"mime"`
	Caption   string `json:"caption"`
	Voice     bool   `json:"voice"`
	Status    int64  `json:"status_msg"`
}

func (s *Service) enqueueMedia(ctx context.Context, c *Client, m *Message, fileID, name, mime string, voice bool) {
	dest := filepath.Join(s.tmpDir(), crypto.RandomToken(8)+"-"+filepath.Base(name))
	c.SendChatAction(ctx, m.Chat.ID, "upload_document")
	if err := c.DownloadFile(ctx, fileID, dest); err != nil {
		s.log.Error("download falhou", "err", err)
		c.SendMessage(ctx, m.Chat.ID, "Falha ao baixar o arquivo: "+err.Error(), m.MessageID)
		return
	}
	label := "📎 Recebido, processando…"
	if voice {
		label = "🎙️ Recebido, transcrevendo…"
	} else if strings.HasPrefix(mime, "image/") {
		label = "🖼️ Recebido, analisando (OCR/visão)…"
	}
	status, _ := c.SendMessage(ctx, m.Chat.ID, label, m.MessageID)
	p := mediaPayload{ChatID: m.Chat.ID, MessageID: m.MessageID, Path: dest, Filename: name, MIME: mime, Caption: m.Caption, Voice: voice, Status: status}
	if _, err := s.db.Enqueue(ctx, TaskMedia, p, database.EnqueueOpts{MaxAttempts: 10}); err != nil {
		s.log.Error("falha ao enfileirar mídia", "err", err)
	}
}

// RegisterTasks wires the media/send queue handlers.
func (s *Service) RegisterTasks(w *queue.Worker) {
	w.Handle(TaskMedia, s.processMedia)
	w.Handle(TaskSend, s.sendQueued)
}

// sendQueued retries a notification; the text was encrypted when it was queued.
func (s *Service) sendQueued(ctx context.Context, raw json.RawMessage) error {
	var p struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return queue.Permanent(err)
	}
	c := s.client()
	if c == nil {
		return queue.Permanentf("telegram não configurado")
	}
	text, err := s.agent.Profile().Open(ctx, p.Text)
	if err != nil {
		return queue.Permanent(err)
	}
	_, err = c.SendMessage(ctx, p.ChatID, text, 0)
	return err
}

func (s *Service) processMedia(ctx context.Context, raw json.RawMessage) error {
	var p mediaPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return queue.Permanent(err)
	}
	data, err := os.ReadFile(p.Path)
	if errors.Is(err, os.ErrNotExist) {
		return queue.Permanentf("arquivo temporário ausente: %s", p.Path)
	}
	if err != nil {
		return err
	}
	mime := p.MIME
	if mime == "" || mime == "application/octet-stream" {
		mime = agent.DetectMIME(p.Filename, data)
	}
	if p.Voice && s.agent.DiaryActive(ctx) != nil { // a spoken answer to the diary: transcribe only
		if done, err := s.diaryVoice(ctx, p, data, mime); done || queue.IsPermanent(err) {
			os.Remove(p.Path)
			return nil
		} else if err != nil {
			return err
		}
	}
	n, err := s.agent.IngestMedia(ctx, agent.MediaInput{
		Data: data, Filename: p.Filename, MIME: mime, Caption: p.Caption, Voice: p.Voice,
		Source: "telegram", SourceRef: fmt.Sprintf("tg:%d:%d", p.ChatID, p.MessageID),
		Meta: map[string]any{"chat_id": p.ChatID},
	})
	c := s.client()
	if err != nil {
		if c != nil && p.Status != 0 {
			if queue.IsPermanent(err) {
				c.EditMessage(ctx, p.ChatID, p.Status, "❌ Falha ao processar: "+err.Error(), false)
				os.Remove(p.Path)
			} else {
				c.EditMessage(ctx, p.ChatID, p.Status, "⏳ Processamento adiado (LLM/rede indisponível). Nova tentativa automática em breve.", false)
			}
		}
		return err
	}
	os.Remove(p.Path)
	if c == nil {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ Salvo: *%s* (#%d, %s)\n", mdEscape(n.Title), n.ID, n.Type)
	if p.Voice {
		b.WriteString("\n_Transcrição:_\n" + extract.Truncate(n.Content, 3000))
	} else if n.Summary != "" {
		b.WriteString("\n" + n.Summary)
	} else {
		b.WriteString("\n" + extract.Truncate(n.Content, 1500))
	}
	if r, _ := n.Meta[agent.MetaReceipt].(string); r != "" { // a purchase document: what became of its warranties
		b.WriteString("\n\n" + r)
	}
	if p.Status != 0 {
		return c.EditMessage(ctx, p.ChatID, p.Status, b.String(), true)
	}
	_, err = c.SendMessage(ctx, p.ChatID, b.String(), p.MessageID)
	return err
}

// diaryVoice transcribes a voice message and records it as the pending diary answer.
func (s *Service) diaryVoice(ctx context.Context, p mediaPayload, data []byte, mime string) (bool, error) {
	text, err := s.agent.Transcribe(ctx, data, p.Filename, mime)
	if err != nil {
		return false, err
	}
	reply, err := s.agent.DiaryAnswer(ctx, text)
	if err != nil {
		return false, nil // no open diary any more: store it as a normal voice note
	}
	if c := s.client(); c != nil {
		msg := "🎙️ _" + extract.Truncate(strings.ReplaceAll(text, "_", " "), 400) + "_\n\n" + reply
		if p.Status != 0 {
			return true, c.EditMessage(ctx, p.ChatID, p.Status, msg, true)
		}
		_, err = c.SendMessage(ctx, p.ChatID, msg, p.MessageID)
		return true, err
	}
	return true, nil
}

// splitItems splits "leite, pão; ovos" into items.
func splitItems(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mdEscape(s string) string {
	return strings.NewReplacer("*", "", "_", " ", "`", "'", "[", "(", "]", ")").Replace(s)
}

func (s *Service) channel(chatID int64) string { return fmt.Sprintf("tg:%d", chatID) }

func (s *Service) provider(ctx context.Context, chatID int64) string {
	v, _, _ := s.db.KVGet(ctx, fmt.Sprintf("tg:model:%d", chatID))
	return v
}

// converse streams an agent answer by progressively editing one message.
func (s *Service) converse(ctx context.Context, c *Client, m *Message, text string) {
	if !s.agent.LLM().Enabled() {
		n, _, err := s.agent.Ingest(ctx, agent.IngestInput{Content: text, Source: "telegram", SourceRef: fmt.Sprintf("tg:%d:%d", m.Chat.ID, m.MessageID), Meta: map[string]any{"quick": true}, Enrich: true})
		if err != nil {
			c.SendMessage(ctx, m.Chat.ID, "Erro: "+err.Error(), m.MessageID)
			return
		}
		c.SendMessage(ctx, m.Chat.ID, fmt.Sprintf("📝 Nota #%d salva (nenhum LLM configurado).", n.ID), m.MessageID)
		return
	}
	c.SendChatAction(ctx, m.Chat.ID, "typing")
	msgID, err := c.SendMessage(ctx, m.Chat.ID, "…", m.MessageID)
	if err != nil {
		s.log.Warn("sendMessage falhou", "err", err)
		return
	}
	var mu sync.Mutex
	var buf strings.Builder
	var tools []string
	lastEdit := time.Now()
	flush := func(final bool) {
		mu.Lock()
		txt := buf.String()
		if len(tools) > 0 && !final {
			txt = "🔧 " + strings.Join(tools, ", ") + "\n\n" + txt
		}
		mu.Unlock()
		if strings.TrimSpace(txt) == "" {
			return
		}
		parts := Split(txt, 4000)
		_ = c.EditMessage(ctx, m.Chat.ID, msgID, parts[0], final)
		if final {
			for _, p := range parts[1:] {
				c.SendMessage(ctx, m.Chat.ID, p, 0)
			}
		}
	}
	res, err := s.agent.Chat(ctx, agent.ChatRequest{Channel: s.channel(m.Chat.ID), Provider: s.provider(ctx, m.Chat.ID), Text: text},
		func(delta string) error {
			mu.Lock()
			buf.WriteString(delta)
			due := time.Since(lastEdit) > 1500*time.Millisecond
			if due {
				lastEdit = time.Now()
			}
			mu.Unlock()
			if due {
				flush(false)
			}
			return nil
		},
		func(ev agent.ChatEvent) {
			if ev.Type == "council_start" {
				mu.Lock()
				tools = append(tools, "🤝 conselho deliberando")
				mu.Unlock()
				flush(false)
				return
			}
			if ev.Type == "tool_call" {
				if d, ok := ev.Data.(map[string]any); ok {
					mu.Lock()
					tools = append(tools, fmt.Sprint(d["name"]))
					mu.Unlock()
				}
			}
		})
	if err != nil {
		s.log.Error("chat Telegram falhou", "err", err)
		c.EditMessage(ctx, m.Chat.ID, msgID, "❌ "+err.Error(), false)
		return
	}
	if strings.TrimSpace(res.Text) == "" {
		mu.Lock()
		buf.WriteString("✅ Feito.")
		mu.Unlock()
	}
	flush(true)
}

const helpText = `*Second Brain* — comandos:
/note <texto> — salva nota
/task <texto> — cria tarefa
/event <texto> — cria evento no Google Calendar (linguagem natural)
/search <termos> — busca híbrida
/tasks — tarefas abertas
/done <id> — conclui tarefa
/brief — gera briefing agora
/hoje — sua rotina de hoje (doses, aulas, hábitos) e próximas datas
/neste_dia — o que você registrou neste dia em anos anteriores
/revisao — revisão espaçada agora (destaques e insights)
/diario — abre o diário guiado (ou /diario pular)
/retro [ano] — retrospectiva do ano
/pessoa <nome> — resumo de uma pessoa (última conversa, pendências)
/crm — quem você não fala há tempo
/viagem [destino] — dossiê da viagem (agenda, reservas, documentos)
/financas [AAAA-MM] — resumo do mês do extrato importado
/compras [itens] — lista de compras (ex.: /compras leite, pão · /compras ok leite · /compras limpar)
/tomei <id> — marca dose ou hábito como feito (desconta o estoque)
/model — escolhe o modelo (ou council para o Conselho)
/reset — limpa o histórico da conversa
Texto livre conversa com o assistente; áudio é transcrito; fotos/PDFs passam por OCR.`

func (s *Service) command(ctx context.Context, c *Client, m *Message) {
	cmd, arg, _ := strings.Cut(strings.TrimSpace(m.Text), " ")
	cmd = strings.ToLower(strings.SplitN(cmd, "@", 2)[0])
	arg = strings.TrimSpace(arg)
	reply := func(t string) { c.SendMessage(ctx, m.Chat.ID, t, m.MessageID) }
	ref := fmt.Sprintf("tg:%d:%d", m.Chat.ID, m.MessageID)
	switch cmd {
	case "/start", "/help":
		reply(helpText + fmt.Sprintf("\n\nSeu user id: `%d`", m.From.ID))
	case "/note":
		if arg == "" {
			reply("Uso: /note <texto>")
			return
		}
		n, _, err := s.agent.Ingest(ctx, agent.IngestInput{Content: arg, Source: "telegram", SourceRef: ref, Enrich: true})
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		reply(fmt.Sprintf("📝 Nota #%d salva: %s", n.ID, n.Title))
	case "/task":
		if arg == "" {
			reply("Uso: /task <texto>")
			return
		}
		n, _, err := s.agent.Ingest(ctx, agent.IngestInput{Type: database.TypeTask, Title: arg, Source: "telegram", SourceRef: ref, Enrich: true})
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		reply(fmt.Sprintf("☑️ Tarefa #%d criada: %s", n.ID, n.Title))
	case "/event":
		ev, _, err := s.agent.CreateEventNL(ctx, arg)
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		loc := s.cfg.Location()
		reply(fmt.Sprintf("📅 Evento criado: %s — %s", ev.Summary, ev.Start.In(loc).Format("02/01 15:04")))
	case "/search":
		hits, err := s.agent.Search(ctx, arg, database.NodeFilter{}, 8)
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		if len(hits) == 0 {
			reply("Nada encontrado.")
			return
		}
		var b strings.Builder
		for _, h := range hits {
			fmt.Fprintf(&b, "• #%d [%s] *%s*\n  %s\n", h.Node.ID, h.Node.Type, mdEscape(h.Node.Title), mdEscape(extract.Truncate(h.Snippet, 140)))
		}
		reply(b.String())
	case "/tasks":
		ts, err := s.db.OpenTasks(ctx, 30)
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		if len(ts) == 0 {
			reply("Nenhuma tarefa aberta 🎉")
			return
		}
		loc := s.cfg.Location()
		var b strings.Builder
		for _, t := range ts {
			due := ""
			if t.DueAt != nil {
				due = " — " + t.DueAt.In(loc).Format("02/01")
			}
			fmt.Fprintf(&b, "• #%d %s%s\n", t.ID, mdEscape(t.Title), due)
		}
		reply(b.String())
	case "/done":
		id, _ := strconv.ParseInt(arg, 10, 64)
		n, err := s.db.GetNode(ctx, id)
		if err != nil || n.Type != database.TypeTask {
			reply("Tarefa não encontrada.")
			return
		}
		s.db.SetStatus(ctx, id, database.StatusDone)
		reply("✅ Concluída: " + n.Title)
	case "/hoje", "/today":
		o, err := s.agent.ProfileOverview(ctx, time.Now(), 7)
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		text := profile.Digest("📋 *Hoje*", o.Today, o.Within(7))
		if text == "" {
			text = "Nada na rotina de hoje. Cadastre medicações, matrículas e datas na página Perfil."
		}
		reply(text)
	case "/tomei", "/feito":
		id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
		if err != nil {
			reply("Uso: /tomei <id> (o número aparece nos lembretes e em /hoje)")
			return
		}
		it, slot, changed, err := s.agent.Profile().CheckNow(ctx, id, time.Now().In(s.cfg.Location()))
		if err != nil {
			reply("Item não encontrado.")
			return
		}
		if !changed {
			reply("Já estava marcado: " + it.Title)
			return
		}
		msg := "✔️ " + it.Title
		if slot != "" {
			msg += " (" + slot + ")"
		}
		if left, ok := profile.DaysLeft(it); ok && left <= 10 {
			msg += fmt.Sprintf("\n⚠️ Estoque para ~%d dias.", int(left))
		}
		reply(msg)
	case "/neste_dia", "/nestedia":
		mem, err := s.agent.OnThisDay(ctx, time.Now())
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		text := agent.FormatOnThisDay(mem)
		if text == "" {
			text = "Nada registrado neste dia nos anos configurados (ON_THIS_DAY_YEARS)."
		}
		reply(text)
	case "/revisao", "/revisão":
		items, err := s.agent.ReviewDue(ctx, time.Now(), 3)
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		text := agent.FormatReviews(items)
		if text == "" {
			text = "Nada para revisar agora. Importe seus destaques do Kindle ou salve insights."
		}
		reply(text)
	case "/diario", "/diário":
		if strings.EqualFold(arg, "pular") {
			s.agent.DiarySkip(ctx)
			reply("📓 Tudo bem, diário de hoje pulado.")
			return
		}
		text, err := s.agent.StartDiary(ctx, time.Now())
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		reply(text)
	case "/retro":
		year := time.Now().In(s.cfg.Location()).Year() - 1
		if y, err := strconv.Atoi(arg); err == nil {
			year = y
		}
		c.SendChatAction(ctx, m.Chat.ID, "typing")
		n, text, err := s.agent.YearReview(ctx, year)
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		reply(fmt.Sprintf("🎆 *Retrospectiva %d* (nota #%d)\n\n%s", year, n.ID, extract.Truncate(text, 3500)))
	case "/pessoa", "/person":
		p, err := s.agent.FindPerson(ctx, arg)
		if err != nil {
			reply(err.Error())
			return
		}
		b, err := s.agent.PersonBriefOf(ctx, p.ID, time.Now())
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		reply(agent.FormatPersonBrief(b, s.cfg.Location(), time.Now()))
	case "/crm":
		list, err := s.agent.StaleContacts(ctx, time.Now(), 10)
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		text := agent.FormatStaleContacts(list, s.cfg.Location(), time.Now())
		if text == "" {
			text = "Ninguém em atraso. 👏 (Ajuste CRM_STALE_DAYS ou use a tag crm em uma pessoa.)"
		}
		reply(text)
	case "/viagem", "/dossie":
		c.SendChatAction(ctx, m.Chat.ID, "typing")
		text, err := s.agent.TravelDossier(ctx, agent.TravelOptions{Destination: arg, Personal: true, SensitiveOK: true})
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		reply(text)
	case "/financas", "/finanças":
		text, err := s.agent.FinanceDigest(ctx, arg)
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		if text == "" {
			text = "Nenhum extrato importado. Use a página Importar → Extrato bancário."
		}
		reply(text)
	case "/compras":
		action, items := "", []string(nil)
		switch lower := strings.ToLower(arg); {
		case arg == "":
		case lower == "limpar":
			action = "clear_done"
		case strings.HasPrefix(lower, "ok "):
			action, items = "check", splitItems(arg[3:])
		case strings.HasPrefix(lower, "tirar "):
			action, items = "remove", splitItems(arg[6:])
		default:
			action, items = "add", splitItems(arg)
		}
		list, err := s.agent.ShoppingList(ctx)
		if action != "" {
			list, err = s.agent.ShoppingEdit(ctx, action, items)
		}
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		reply(agent.FormatShopping(list))
	case "/brief":
		if s.Briefing == nil {
			reply("Briefing indisponível.")
			return
		}
		c.SendChatAction(ctx, m.Chat.ID, "typing")
		b, err := s.Briefing(ctx)
		if err != nil {
			reply("Erro: " + err.Error())
			return
		}
		reply(b)
	case "/model":
		key := fmt.Sprintf("tg:model:%d", m.Chat.ID)
		lm := s.agent.LLM()
		switch arg {
		case "":
			cur := s.provider(ctx, m.Chat.ID)
			if cur == "" {
				cur = "auto (tarefa Telegram: " + lm.Route(llm.TaskTelegram) + ")"
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Modelo atual: `%s`\n\nOpções:\n• `/model auto` — segue a configuração do painel\n• `/model council` — 🤝 Conselho (modelos debatem)\n", cur)
			for _, e := range lm.Catalog() {
				if e.Configured {
					fmt.Fprintf(&b, "• `/model %s`\n", e.Spec())
				}
			}
			reply(b.String())
		case "auto":
			s.db.KVDelete(ctx, key)
			reply("Usando o modelo configurado no painel para o Telegram.")
		case llm.SpecCouncil:
			s.db.KVSet(ctx, key, arg)
			reply("🤝 Conselho ativado: " + strings.Join(lm.CouncilSetup().Members, ", "))
		default:
			if _, _, err := lm.Resolve(arg); err != nil {
				reply("Erro: " + err.Error())
				return
			}
			s.db.KVSet(ctx, key, arg)
			reply("Modelo definido: " + arg)
		}
	case "/reset":
		s.db.ClearChat(ctx, s.channel(m.Chat.ID))
		reply("🧹 Histórico limpo.")
	default:
		s.converse(ctx, c, m, m.Text)
	}
}

// Notify sends text to every allowed user; failures are queued for retry.
func (s *Service) Notify(ctx context.Context, text string) error {
	c := s.client()
	if c == nil {
		return nil
	}
	var (
		errs   []error
		sealed string
	)
	for _, id := range s.AllowedIDs() {
		if _, err := c.SendMessage(ctx, id, text, 0); err != nil {
			errs = append(errs, err)
			// Reports and reminders may carry personal data: the retry copy is encrypted.
			if sealed == "" {
				var serr error
				if sealed, serr = s.agent.Profile().Seal(ctx, text); serr != nil {
					s.log.Error("notificação não reenfileirada (cofre indisponível)", "err", serr)
					continue
				}
			}
			s.db.Enqueue(ctx, TaskSend, map[string]any{"chat_id": id, "text": sealed}, database.EnqueueOpts{MaxAttempts: 12})
		}
	}
	return errors.Join(errs...)
}

// SendDocument uploads a file to a chat.
func (s *Service) SendDocument(ctx context.Context, chatID int64, name string, r io.Reader, caption string) error {
	c := s.client()
	if c == nil {
		return errors.New("telegram não configurado")
	}
	return c.SendDocument(ctx, chatID, name, r, caption)
}

// PurgeTemp deletes temporary media files older than age.
func (s *Service) PurgeTemp(age time.Duration) int {
	entries, err := os.ReadDir(s.tmpDir())
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && time.Since(info.ModTime()) > age {
			if os.Remove(filepath.Join(s.tmpDir(), e.Name())) == nil {
				n++
			}
		}
	}
	return n
}

// ---- pairing ----

const pendingKey = "telegram.pending"

// PendingUser is someone who messaged the bot but is not authorized yet.
type PendingUser struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	FirstName string    `json:"first_name"`
	At        time.Time `json:"at"`
}

// recordPending remembers unauthorized senders (for one-click authorization in the
// web panel) and answers /start with the user's id. Nothing else is revealed.
func (s *Service) recordPending(ctx context.Context, c *Client, m *Message) {
	if m.From == nil {
		return
	}
	s.log.Warn("mensagem de usuário não autorizado ignorada", "user_id", m.From.ID, "username", m.From.Username)
	var list []PendingUser
	_, _ = s.db.KVGetJSON(ctx, pendingKey, &list)
	kept := list[:0]
	for _, p := range list {
		if p.ID != m.From.ID && time.Since(p.At) < 7*24*time.Hour {
			kept = append(kept, p)
		}
	}
	kept = append(kept, PendingUser{ID: m.From.ID, Username: m.From.Username, FirstName: m.From.FirstName, At: time.Now()})
	if len(kept) > 20 {
		kept = kept[len(kept)-20:]
	}
	_ = s.db.KVSetJSON(ctx, pendingKey, kept)
	if strings.HasPrefix(strings.TrimSpace(m.Text), "/start") {
		c.SendMessage(ctx, m.Chat.ID, fmt.Sprintf("👋 Olá! Seu ID do Telegram é `%d`.\n\nPara liberar o acesso, abra o painel web do Second Brain → *Configurações → Telegram* e clique em *Autorizar* ao lado do seu nome.", m.From.ID), m.MessageID)
	}
}

// Pending lists users awaiting authorization.
func (s *Service) Pending(ctx context.Context) []PendingUser {
	var list []PendingUser
	_, _ = s.db.KVGetJSON(ctx, pendingKey, &list)
	allowed := map[int64]bool{}
	for _, id := range s.AllowedIDs() {
		allowed[id] = true
	}
	out := list[:0]
	for _, p := range list {
		if !allowed[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// Authorize adds id to ALLOWED_TELEGRAM_USER_IDS and greets the user.
func (s *Service) Authorize(ctx context.Context, id int64) error {
	ids := s.AllowedIDs()
	for _, x := range ids {
		if x == id {
			return nil
		}
	}
	var parts []string
	for _, x := range append(ids, id) {
		parts = append(parts, strconv.FormatInt(x, 10))
	}
	if err := s.cfg.Update(map[string]string{"ALLOWED_TELEGRAM_USER_IDS": strings.Join(parts, ",")}); err != nil {
		return err
	}
	s.log.Info("usuário do Telegram autorizado", "user_id", id)
	if c := s.client(); c != nil {
		go func() { // greet without blocking the web request
			gctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c.SendMessage(gctx, id, "✅ Acesso liberado! Envie uma mensagem, um áudio ou uma foto. Use /help para ver os comandos.", 0)
		}()
	}
	return nil
}

// Revoke removes id from the allow-list.
func (s *Service) Revoke(id int64) error {
	var parts []string
	for _, x := range s.AllowedIDs() {
		if x != id {
			parts = append(parts, strconv.FormatInt(x, 10))
		}
	}
	return s.cfg.Update(map[string]string{"ALLOWED_TELEGRAM_USER_IDS": strings.Join(parts, ",")})
}

// Check validates the bot token and returns the bot account.
func (s *Service) Check(ctx context.Context) (*User, error) {
	c := s.client()
	if c == nil {
		return nil, errors.New("token do bot não configurado")
	}
	return c.GetMe(ctx)
}
