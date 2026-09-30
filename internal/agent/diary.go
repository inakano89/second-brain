package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/queue"
)

// Guided diary: at night the bot asks two or three short questions; the next messages (text or
// voice) answer them one by one and end up in the note "Diário DD/MM/AAAA".

const (
	diaryKey    = "diary.pending"
	diarySource = "diary"
	// diaryWindow is how long after the questions a message still counts as an answer: short, so
	// that a normal conversation the next morning is not swallowed by an old diary.
	diaryWindow = 4 * time.Hour
)

// DiaryState is a diary session waiting for answers.
type DiaryState struct {
	Date      string    `json:"date"` // YYYY-MM-DD
	Questions []string  `json:"questions"`
	Answers   []string  `json:"answers"`
	Expires   time.Time `json:"expires"`
}

// Next is the question waiting for an answer ("" when all were answered).
func (s *DiaryState) Next() string {
	if len(s.Answers) < len(s.Questions) {
		return s.Questions[len(s.Answers)]
	}
	return ""
}

var diaryMu sync.Mutex

var (
	diaryOpeners = []string{"Como foi o seu dia, em uma frase?"}
	diaryMiddle  = []string{
		"O que você aprendeu ou percebeu hoje?",
		"Do que você se orgulha hoje?",
		"O que foi mais difícil hoje e como você lidou com isso?",
		"O que te deixou mais energizado hoje?",
		"Que conversa ou encontro marcou o seu dia?",
	}
	diaryClosers = []string{
		"Pelo que você é grato hoje?",
		"Qual é a coisa mais importante para amanhã?",
		"O que você faria diferente se o dia recomeçasse?",
	}
)

// diaryFallback picks a stable but varying trio of questions for a day.
func diaryFallback(day time.Time) []string {
	n := day.YearDay()
	return []string{diaryOpeners[0], diaryMiddle[n%len(diaryMiddle)], diaryClosers[n%len(diaryClosers)]}
}

// DiaryQuestions writes tonight's questions: a model tailors them to the day (agenda, finished
// tasks, notes); without one, a varied fixed set is used.
func (a *Agent) DiaryQuestions(ctx context.Context, now time.Time) []string {
	loc := a.cfg.Location()
	fallback := diaryFallback(now.In(loc))
	if !a.llm.Enabled() {
		return fallback
	}
	start := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
	var data strings.Builder
	if evs, err := a.EventsBetween(ctx, start, start.AddDate(0, 0, 1)); err == nil {
		for _, e := range evs {
			fmt.Fprintf(&data, "Evento: %s\n", e.Summary)
		}
	}
	if done, err := a.db.UpdatedBetween(ctx, []string{database.TypeTask}, database.StatusDone, start, now.Add(time.Minute), 10); err == nil {
		for _, t := range done {
			fmt.Fprintf(&data, "Tarefa concluída: %s\n", t.Title)
		}
	}
	if notes, err := a.db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypeNote, database.TypeInsight}, From: &start, KnownDate: true, Limit: 8}); err == nil {
		for _, n := range notes {
			if n.Source == "routine" || n.Source == MemorySource || n.Source == diarySource {
				continue
			}
			fmt.Fprintf(&data, "Anotação: %s\n", n.Title)
		}
	}
	if strings.TrimSpace(data.String()) == "" {
		return fallback
	}
	text, err := a.Ask(ctx, "diary", `Você conduz um diário noturno. Escreva 3 perguntas curtas, calorosas e específicas para o dia descrito, em português, uma por linha, sem numeração nem introdução. A primeira pergunta é sobre o dia como um todo; a última olha para amanhã ou para gratidão. Se o dia não tiver dados suficientes, faça perguntas gerais.`, data.String())
	if err != nil {
		return fallback
	}
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "-*•0123456789.) "))
		if len([]rune(ln)) >= 8 && strings.HasSuffix(ln, "?") {
			out = append(out, ln)
		}
	}
	if len(out) < 2 {
		return fallback
	}
	return out[:min(len(out), 3)]
}

// StartDiary opens tonight's session and returns the message that asks the questions.
func (a *Agent) StartDiary(ctx context.Context, now time.Time) (string, error) {
	loc := a.cfg.Location()
	qs := a.DiaryQuestions(ctx, now)
	st := DiaryState{Date: now.In(loc).Format("2006-01-02"), Questions: qs, Expires: now.Add(diaryWindow)}
	diaryMu.Lock()
	defer diaryMu.Unlock()
	if err := a.db.KVSetJSON(ctx, diaryKey, st); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("📓 *Diário de hoje*\nResponda uma pergunta por mensagem, por texto ou áudio. Para pular hoje: /diario pular\n\n")
	fmt.Fprintf(&b, "1. %s", qs[0])
	for i, q := range qs[1:] {
		fmt.Fprintf(&b, "\n_%d. %s_", i+2, q)
	}
	return b.String(), nil
}

// DiaryActive returns the session waiting for answers, if any.
func (a *Agent) DiaryActive(ctx context.Context) *DiaryState {
	var st DiaryState
	if ok, _ := a.db.KVGetJSON(ctx, diaryKey, &st); !ok || st.Date == "" || time.Now().After(st.Expires) || st.Next() == "" {
		return nil
	}
	return &st
}

// DiarySkip drops tonight's session.
func (a *Agent) DiarySkip(ctx context.Context) error { return a.db.KVDelete(ctx, diaryKey) }

// DiaryAnswer records text as the answer to the pending question, saves the diary note and
// returns what to tell the user: the next question, or a confirmation after the last one.
func (a *Agent) DiaryAnswer(ctx context.Context, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("resposta vazia")
	}
	diaryMu.Lock()
	defer diaryMu.Unlock()
	var st DiaryState
	if ok, _ := a.db.KVGetJSON(ctx, diaryKey, &st); !ok || st.Next() == "" || time.Now().After(st.Expires) {
		return "", errors.New("não há diário aberto")
	}
	st.Answers = append(st.Answers, text)
	day, _ := time.ParseInLocation("2006-01-02", st.Date, a.cfg.Location())
	var b strings.Builder
	for i, ans := range st.Answers {
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", st.Questions[i], ans)
	}
	n, _, err := a.Ingest(ctx, IngestInput{
		Type: database.TypeNote, Title: "Diário " + day.Format("02/01/2006"), Content: strings.TrimSpace(b.String()),
		Source: diarySource, SourceRef: "diary:" + st.Date, Tags: []string{"diario"}, Enrich: true,
	})
	if err != nil {
		return "", err
	}
	if next := st.Next(); next != "" {
		if err := a.db.KVSetJSON(ctx, diaryKey, st); err != nil {
			return "", err
		}
		return fmt.Sprintf("📓 Anotado. %d/%d: %s", len(st.Answers)+1, len(st.Questions), next), nil
	}
	_ = a.db.KVDelete(ctx, diaryKey)
	return fmt.Sprintf("📓 Diário de %s salvo (#%d). Boa noite! 🌙", day.Format("02/01"), n.ID), nil
}

// Transcribe turns audio into text without storing anything.
func (a *Agent) Transcribe(ctx context.Context, data []byte, filename, mime string) (string, error) {
	if !a.llm.Enabled() {
		return "", queue.Permanent(llm.ErrNoProvider)
	}
	text, err := a.llm.Transcribe(ctx, data, filename, mime)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", queue.Permanent(errors.New("transcrição vazia"))
	}
	return extract.Truncate(text, 8000), nil
}
