package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/inakano89/second-brain/internal/database"
)

// Shopping list: one checklist note ("Lista de compras") kept by the chat, Telegram and the web.

const (
	shoppingSource = "shopping"
	shoppingRef    = "list:default"
	shoppingTitle  = "Lista de compras"
)

// ShoppingItem is one line of the list.
type ShoppingItem struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

func parseShopping(content string) []ShoppingItem {
	var out []ShoppingItem
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "- [ ] "):
			out = append(out, ShoppingItem{Text: strings.TrimSpace(ln[6:])})
		case strings.HasPrefix(ln, "- [x] "), strings.HasPrefix(ln, "- [X] "):
			out = append(out, ShoppingItem{Text: strings.TrimSpace(ln[6:]), Done: true})
		}
	}
	return out
}

func renderShopping(items []ShoppingItem) string {
	var b strings.Builder
	for _, it := range items {
		if it.Done {
			b.WriteString("- [x] " + it.Text + "\n")
		} else {
			b.WriteString("- [ ] " + it.Text + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// ShoppingList returns the current list.
func (a *Agent) ShoppingList(ctx context.Context) ([]ShoppingItem, error) {
	n, err := a.db.GetNodeBySource(ctx, shoppingSource, shoppingRef)
	if errors.Is(err, database.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseShopping(n.Content), nil
}

func sameItem(a, b string) bool { return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) }

// ShoppingEdit changes the list. action is add, remove, check (bought), uncheck, clear_done or
// clear_all. Items are matched case-insensitively; remove and check also accept a unique fragment.
func (a *Agent) ShoppingEdit(ctx context.Context, action string, items []string) ([]ShoppingItem, error) {
	list, err := a.ShoppingList(ctx)
	if err != nil {
		return nil, err
	}
	find := func(q string) int {
		q = strings.TrimSpace(q)
		for i, it := range list {
			if sameItem(it.Text, q) {
				return i
			}
		}
		hit := -1
		for i, it := range list {
			if q != "" && strings.Contains(strings.ToLower(it.Text), strings.ToLower(q)) {
				if hit >= 0 {
					return -1 // ambiguous
				}
				hit = i
			}
		}
		return hit
	}
	switch action {
	case "add":
		for _, q := range items {
			if q = strings.TrimSpace(q); q == "" {
				continue
			}
			if i := find(q); i >= 0 && sameItem(list[i].Text, q) {
				list[i].Done = false
				continue
			}
			list = append(list, ShoppingItem{Text: q})
		}
	case "remove", "check", "uncheck":
		for _, q := range items {
			i := find(q)
			if i < 0 {
				return nil, fmt.Errorf("não achei “%s” na lista (ou há mais de um parecido)", strings.TrimSpace(q))
			}
			switch action {
			case "remove":
				list = append(list[:i], list[i+1:]...)
			case "check":
				list[i].Done = true
			default:
				list[i].Done = false
			}
		}
	case "clear_done":
		kept := list[:0]
		for _, it := range list {
			if !it.Done {
				kept = append(kept, it)
			}
		}
		list = kept
	case "clear_all":
		list = nil
	default:
		return nil, fmt.Errorf("ação desconhecida: %s", action)
	}
	if _, _, err := a.Ingest(ctx, IngestInput{
		Type: database.TypeNote, Title: shoppingTitle, Content: renderShopping(list), Source: shoppingSource, SourceRef: shoppingRef,
		Tags: []string{"compras"}, Meta: map[string]any{"enriched": true},
	}); err != nil {
		return nil, err
	}
	return list, nil
}

// FormatShopping renders the list for Telegram.
func FormatShopping(list []ShoppingItem) string {
	if len(list) == 0 {
		return "🛒 A lista de compras está vazia. Adicione com /compras leite, pão"
	}
	var b strings.Builder
	b.WriteString("🛒 *Lista de compras*\n")
	for _, it := range list {
		if it.Done {
			fmt.Fprintf(&b, "✅ ~%s~\n", oneLine(it.Text))
		} else {
			fmt.Fprintf(&b, "⬜ %s\n", oneLine(it.Text))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
