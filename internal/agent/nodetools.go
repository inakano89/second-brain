package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
)

// toolUpdateNode edits an existing graph node (notes, tasks, people, events…).
const toolUpdateNode = "update_node"

func strList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func (a *Agent) updateNodeTool() llm.Tool {
	return llm.Tool{Name: toolUpdateNode,
		Description: "Edita um nó existente do Second Brain (nota, tarefa, pessoa, evento…). Só os campos informados mudam; use get_node/search_brain antes para obter o ID e ver o conteúdo atual. " +
			"Prefira `append` a `content` para acrescentar informação sem perder o texto existente; `content` substitui tudo. " +
			"Para PESSOAS, `emails`, `phones`, `company` e `birthday` também são editáveis (listas substituem as atuais; texto vazio limpa). Não apaga nós.",
		Parameters: obj(map[string]any{
			"id":       integer("ID do nó"),
			"title":    str("Novo título"),
			"content":  str("Novo conteúdo em Markdown (substitui o atual)"),
			"append":   str("Texto acrescentado ao final do conteúdo atual"),
			"summary":  str("Novo resumo"),
			"tags":     strList("Tags (substituem as atuais)"),
			"add_tags": strList("Tags acrescentadas às atuais"),
			"status":   map[string]any{"type": "string", "enum": []string{"open", "done"}, "description": "Status (tarefas)"},
			"due":      str("Prazo YYYY-MM-DD ou YYYY-MM-DDTHH:MM; \"none\" remove"),
			"emails":   strList("E-mails da pessoa (substituem os atuais)"),
			"phones":   strList("Telefones da pessoa (substituem os atuais)"),
			"company":  str("Empresa/organização da pessoa"),
			"birthday": str("Aniversário da pessoa: AAAA-MM-DD (ou --MM-DD sem ano)"),
		}, "id")}
}

// personKeys are the person-only meta fields the chat may read and edit.
var personKeys = []string{"emails", "phones", "company", "birthday"}

func personDetails(n *database.Node) map[string]any {
	if n.Type != database.TypePerson {
		return nil
	}
	out := map[string]any{}
	for _, k := range personKeys {
		if v, ok := n.Meta[k]; ok && v != nil && v != "" {
			out[k] = v
		}
	}
	return out
}

// applyPersonArgs writes the person fields present in args into n.Meta.
func applyPersonArgs(n *database.Node, args toolArgs) error {
	for _, k := range []string{"emails", "phones"} {
		if _, ok := args[k]; !ok {
			continue
		}
		var list []string
		for _, s := range args.strs(k) {
			if s = strings.TrimSpace(s); s != "" {
				list = append(list, s)
			}
		}
		if len(list) == 0 {
			delete(n.Meta, k)
		} else {
			n.Meta[k] = list
		}
	}
	if _, ok := args["company"]; ok {
		if v := args.str("company"); v == "" {
			delete(n.Meta, "company")
		} else {
			n.Meta["company"] = v
		}
	}
	if _, ok := args["birthday"]; ok {
		v := args.str("birthday")
		switch {
		case v == "":
			delete(n.Meta, "birthday")
		case validBirthday(v):
			n.Meta["birthday"] = v
		default:
			return fmt.Errorf("aniversário inválido %q: use AAAA-MM-DD ou --MM-DD", v)
		}
	}
	return nil
}

func validBirthday(s string) bool {
	if strings.HasPrefix(s, "--") {
		_, err := time.Parse("2006-01-02", "2000-"+s[2:])
		return err == nil
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func (a *Agent) updateNode(ctx context.Context, args toolArgs) (any, error) {
	n, err := a.db.GetNode(ctx, args.int("id"))
	if err != nil {
		return nil, err
	}
	if n.Type == database.TypeHealth {
		return nil, errors.New("nós de saúde são gerados automaticamente e não podem ser editados")
	}
	loc := a.cfg.Location()
	changed := []string{}
	mark := func(f string) { changed = append(changed, f) }

	if v := args.str("title"); v != "" && v != n.Title {
		n.Title = extract.Truncate(v, 200)
		mark("title")
	}
	if _, ok := args["content"]; ok {
		if v, _ := args["content"].(string); v != n.Content {
			n.Content = v
			mark("content")
		}
	}
	if v := args.str("append"); v != "" {
		n.Content = strings.TrimRight(n.Content, "\n") + "\n\n" + v
		mark("content")
	}
	if _, ok := args["summary"]; ok {
		n.Summary = args.str("summary")
		mark("summary")
	}
	if _, ok := args["tags"]; ok {
		n.Tags = args.strs("tags")
		mark("tags")
	}
	if add := args.strs("add_tags"); len(add) > 0 {
		n.Tags = append(n.Tags, add...)
		mark("tags")
	}
	if st := args.str("status"); st != "" {
		if n.Type != database.TypeTask {
			return nil, errors.New("status só se aplica a tarefas")
		}
		if st != database.StatusOpen && st != database.StatusDone {
			return nil, fmt.Errorf("status inválido %q", st)
		}
		n.Status = st
		mark("status")
	}
	if d := args.str("due"); d != "" {
		if strings.EqualFold(d, "none") {
			n.DueAt = nil
		} else {
			t, err := parseLocal(d, loc)
			if err != nil {
				return nil, err
			}
			n.DueAt = &t
		}
		mark("due")
	}
	if n.Type == database.TypePerson {
		before := len(changed)
		for _, k := range personKeys {
			if _, ok := args[k]; ok {
				mark(k)
			}
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		if err := applyPersonArgs(n, args); err != nil {
			return nil, err
		}
		if len(changed) > before {
			n.Meta["auto"] = false // hand-edited: keep out of the "lonely person" cleanup
		}
	} else {
		for _, k := range personKeys {
			if _, ok := args[k]; ok {
				return nil, fmt.Errorf("%s só se aplica a pessoas", k)
			}
		}
	}
	if len(changed) == 0 {
		return nil, errors.New("nada para alterar: informe ao menos um campo")
	}
	if err := a.db.UpdateNode(ctx, n); err != nil {
		return nil, err
	}
	if err := a.QueueEnrich(ctx, n.ID); err != nil {
		a.log.Warn("falha ao enfileirar enriquecimento", "id", n.ID, "err", err)
	}
	res := map[string]any{"updated": brief(n, loc), "changed": changed}
	if p := personDetails(n); len(p) > 0 {
		res["person"] = p
	}
	if n.Source == "contacts" {
		res["aviso"] = "contato sincronizado do Google: a edição fica só aqui e pode ser sobrescrita se o contato mudar no Google"
	}
	return res, nil
}
