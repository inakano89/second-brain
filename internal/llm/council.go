package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Opinion is one council member's answer in a deliberation round.
type Opinion struct {
	Label    string `json:"label"` // anonymous label shown to other members ("Modelo A")
	Spec     string `json:"spec"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Round    int    `json:"round"`
	Text     string `json:"text"`
	Err      string `json:"error,omitempty"`
}

// CouncilConfig is the resolved council composition.
type CouncilConfig struct {
	Members []string // "provider:model" specs
	Judge   string
	Rounds  int
}

// ErrCouncilTooSmall means fewer than two council members are available.
var ErrCouncilTooSmall = errors.New("llm: conselho precisa de ao menos 2 modelos configurados")

const judgeInstructions = `Você é o moderador de um conselho de modelos de IA que debateram a solicitação do usuário.
Abaixo estão as posições finais de cada membro. Produza a MELHOR resposta final ao usuário:
- combine os acertos e descarte erros;
- quando houver divergência relevante, decida e explique em uma frase o porquê;
- seja direto e acionável; não descreva o processo de debate a menos que seja útil.`

// CouncilSetup resolves LLM_COUNCIL_* settings against configured providers.
func (m *Manager) CouncilSetup() CouncilConfig {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	var cc CouncilConfig
	seen := map[string]bool{}
	for _, raw := range cfg.GetList("LLM_COUNCIL_MEMBERS") {
		spec := m.fullSpec(raw)
		if spec == "" || seen[spec] {
			continue
		}
		seen[spec] = true
		cc.Members = append(cc.Members, spec)
	}
	cc.Judge = m.fullSpec(cfg.Get("LLM_COUNCIL_JUDGE"))
	if cc.Judge == "" && len(cc.Members) > 0 {
		cc.Judge = cc.Members[0]
	}
	cc.Rounds = min(max(cfg.GetInt("LLM_COUNCIL_ROUNDS", 1), 0), 3)
	return cc
}

// fullSpec expands "provider" to "provider:defaultModel"; returns "" if unconfigured.
func (m *Manager) fullSpec(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == SpecAuto {
		raw = m.DefaultProvider()
	}
	p, model, _ := strings.Cut(raw, ":")
	m.mu.RLock()
	prov, ok := m.providers[p]
	m.mu.RUnlock()
	if !ok {
		return ""
	}
	if model == "" {
		model = prov.Model()
	}
	return p + ":" + model
}

func councilPurpose(purpose, role string) string {
	base, _, _ := strings.Cut(purpose, ":")
	if base == "" {
		base = "council"
	}
	return base + ":" + role
}

func label(i int) string { return fmt.Sprintf("Modelo %c", 'A'+i) }

// Deliberate asks every member in parallel, then runs critique rounds in which
// each member reads the others' (anonymized) answers and revises its own.
// onOpinion is called from multiple goroutines as answers arrive.
func (m *Manager) Deliberate(ctx context.Context, req Request, onOpinion func(Opinion)) ([]Opinion, CouncilConfig, error) {
	cc := m.CouncilSetup()
	if len(cc.Members) < 2 {
		return nil, cc, ErrCouncilTooSmall
	}
	base := req
	base.Tools = nil
	base.Purpose = councilPurpose(req.Purpose, "member")
	if base.MaxTokens <= 0 {
		base.MaxTokens = 8000
	}
	n := len(cc.Members)
	latest := make([]string, n)
	alive := make([]bool, n)
	var all []Opinion
	var mu sync.Mutex

	run := func(round int, build func(i int) Request) {
		// Prompts and participation are fixed before any goroutine starts.
		active := append([]bool(nil), alive...)
		reqs := make([]Request, n)
		for i := range cc.Members {
			if round == 0 || active[i] {
				reqs[i] = build(i)
			}
		}
		var wg sync.WaitGroup
		for i, spec := range cc.Members {
			if round > 0 && !active[i] {
				continue
			}
			wg.Add(1)
			go func(i int, spec string) {
				defer wg.Done()
				p, model, _ := strings.Cut(spec, ":")
				op := Opinion{Label: label(i), Spec: spec, Provider: p, Model: model, Round: round}
				resp, err := m.streamOne(ctx, spec, reqs[i], nil)
				if err != nil {
					op.Err = err.Error()
				} else {
					op.Text = strings.TrimSpace(resp.Text)
				}
				mu.Lock()
				if err == nil && op.Text != "" {
					latest[i], alive[i] = op.Text, true
				} else if round == 0 {
					alive[i] = false
				}
				all = append(all, op)
				mu.Unlock()
				if onOpinion != nil {
					onOpinion(op)
				}
			}(i, spec)
		}
		wg.Wait()
	}

	run(0, func(int) Request { return base })
	count := func() int {
		c := 0
		for _, a := range alive {
			if a {
				c++
			}
		}
		return c
	}
	if count() == 0 {
		return all, cc, errors.New("llm: nenhum membro do conselho respondeu")
	}
	for r := 1; r <= cc.Rounds && count() >= 2; r++ {
		snapshot := append([]string(nil), latest...)
		live := append([]bool(nil), alive...)
		run(r, func(i int) Request {
			var others strings.Builder
			for j, t := range snapshot {
				if j != i && live[j] {
					fmt.Fprintf(&others, "### %s\n%s\n\n", label(j), t)
				}
			}
			q := base
			q.Messages = append(append([]Message{}, base.Messages...),
				Message{Role: RoleAssistant, Content: snapshot[i]},
				Message{Role: RoleUser, Content: "Outros membros do conselho responderam à mesma solicitação:\n\n" + others.String() +
					"Avalie criticamente todas as respostas, inclusive a sua: aponte erros, lacunas e pontos fortes. " +
					"Depois apresente sua resposta revisada e FINAL (completa, não apenas as mudanças)."})
			return q
		})
	}
	return all, cc, nil
}

// FinalOpinions returns the latest successful answer of each member.
func FinalOpinions(ops []Opinion) []Opinion {
	best := map[string]Opinion{}
	var order []string
	for _, o := range ops {
		if o.Err != "" || o.Text == "" {
			continue
		}
		prev, ok := best[o.Label]
		if !ok {
			order = append(order, o.Label)
		}
		if !ok || o.Round >= prev.Round {
			best[o.Label] = o
		}
	}
	out := make([]Opinion, 0, len(order))
	for _, l := range order {
		out = append(out, best[l])
	}
	return out
}

// SynthesisPrompt builds the moderator instructions with the members' final positions.
func SynthesisPrompt(ops []Opinion) string {
	var b strings.Builder
	b.WriteString(judgeInstructions)
	b.WriteString("\n\n<posicoes_do_conselho>\n")
	for _, o := range FinalOpinions(ops) {
		fmt.Fprintf(&b, "### %s\n%s\n\n", o.Label, o.Text)
	}
	b.WriteString("</posicoes_do_conselho>")
	return b.String()
}

// Council deliberates and streams the moderator's synthesis. With fewer than two
// configured members it degrades to a single model call.
func (m *Manager) Council(ctx context.Context, req Request, onText StreamFunc, onOpinion func(Opinion)) (*Response, error) {
	ops, cc, err := m.Deliberate(ctx, req, onOpinion)
	if errors.Is(err, ErrCouncilTooSmall) {
		return m.streamOne(ctx, cc.Judge, req, onText)
	}
	if err != nil {
		return nil, err
	}
	judge := req
	judge.Purpose = councilPurpose(req.Purpose, "judge")
	judge.System = strings.TrimSpace(req.System + "\n\n" + SynthesisPrompt(ops))
	resp, err := m.streamOne(ctx, cc.Judge, judge, onText)
	if err != nil {
		return nil, err
	}
	resp.Council = ops
	return resp, nil
}
