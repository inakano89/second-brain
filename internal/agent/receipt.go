package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
)

// Receipts to warranties: a photo (or PDF) of an invoice becomes "Garantia ou contrato" items in
// the personal profile, with the expiry date that feeds the upcoming-dates alerts.

// MetaReceipt records what the receipt reader did with a media node.
const MetaReceipt = "receipt"

var receiptHintRe = regexp.MustCompile(`(?i)(garantia|nota[ -]?fiscal|\bnfc?-?e?\b|recibo|cupom fiscal|danfe|comprovante de compra|receipt|invoice)`)

var receiptTags = map[string]bool{"recibo": true, "nota-fiscal": true, "nota fiscal": true, "cupom-fiscal": true, "danfe": true, "comprovante": true, "receipt": true}

// looksLikeReceipt decides from the caption, file name and tags whether media is a purchase document.
func looksLikeReceipt(caption, filename string, n *database.Node) bool {
	if receiptHintRe.MatchString(caption) || receiptHintRe.MatchString(filename) {
		return true
	}
	for _, t := range n.Tags {
		if receiptTags[t] {
			return true
		}
	}
	return false
}

type receiptItem struct {
	Product        string  `json:"product"`
	Brand          string  `json:"brand"`
	Price          float64 `json:"price"`
	WarrantyMonths int     `json:"warranty_months"`
}

type receiptData struct {
	Store   string        `json:"store"`
	Date    string        `json:"date"`
	Total   float64       `json:"total"`
	Invoice string        `json:"invoice"`
	Items   []receiptItem `json:"durable_items"`
}

const receiptSystem = `Você lê o texto de uma nota fiscal, recibo ou cupom e extrai o que tem garantia.
Responda SOMENTE JSON: {"store":"loja","date":"YYYY-MM-DD da compra","total":0,"invoice":"número da nota","durable_items":[{"product":"nome do produto","brand":"marca","price":0,"warranty_months":0}]}
"durable_items" só traz bens duráveis (eletrodomésticos, eletrônicos, móveis, ferramentas, bicicletas, relógios, colchões…). Compras de consumo (mercado, restaurante, farmácia, combustível) NÃO entram: deixe a lista vazia.
warranty_months é o prazo escrito no documento; 0 se não houver. Não invente dados.`

// ReceiptWarranties reads the text of a purchase document node and registers a warranty for each
// durable product. It returns a message for the user ("" when nothing applies). Items are written
// to the profile only when PROFILE_AI_WRITE allows; otherwise the message says what was found.
func (a *Agent) ReceiptWarranties(ctx context.Context, n *database.Node) (string, error) {
	if !a.llm.Enabled() || strings.TrimSpace(n.Content) == "" {
		return "", nil
	}
	loc := a.cfg.Location()
	var rd receiptData
	err := a.llm.CompleteJSON(ctx, "", llm.Request{
		Purpose: "receipt", System: receiptSystem, MaxTokens: 2000, Effort: "low",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "Data de hoje: " + time.Now().In(loc).Format("2006-01-02") + "\n\n" + extract.Truncate(n.Content, 9000)}},
	}, &rd)
	if err != nil {
		return "", err
	}
	if len(rd.Items) == 0 {
		return "", nil
	}
	bought := n.CreatedAt.In(loc)
	if t, err := time.ParseInLocation("2006-01-02", rd.Date, loc); err == nil && !t.After(time.Now().AddDate(0, 0, 1)) {
		bought = t
	}
	defMonths := a.cfg.GetInt("WARRANTY_DEFAULT_MONTHS", 12)
	canWrite := a.profile.AIWrite()
	var lines []string
	saved := 0
	for i, it := range rd.Items {
		if i >= 6 {
			break
		}
		name := strings.TrimSpace(strings.TrimSpace(it.Brand + " " + it.Product))
		if name == "" {
			continue
		}
		months, assumed := it.WarrantyMonths, false
		if months <= 0 || months > 120 {
			months, assumed = defMonths, true
		}
		until := bought.AddDate(0, months, 0)
		if until.Before(time.Now().AddDate(0, 0, -30)) {
			continue // long expired: nothing to alert about
		}
		var notes []string
		if rd.Invoice != "" {
			notes = append(notes, "Nota fiscal "+rd.Invoice)
		}
		notes = append(notes, "comprado em "+bought.Format("02/01/2006"))
		if it.Price > 0 {
			notes = append(notes, fmt.Sprintf("R$ %.2f", it.Price))
		}
		if assumed {
			notes = append(notes, fmt.Sprintf("prazo de %d meses presumido: confira o certificado", months))
		}
		line := fmt.Sprintf("• *%s* — garantia até %s (%d meses%s)", oneLine(name), until.Format("02/01/2006"), months, map[bool]string{true: ", presumido", false: ""}[assumed])
		if canWrite {
			if _, _, err := a.profile.UpsertFromAI(ctx, "warranty", name, map[string]string{
				"until": until.Format("2006-01-02"), "store": strings.TrimSpace(rd.Store), "notes": strings.Join(notes, " · ") + fmt.Sprintf(" · origem: nota #%d", n.ID),
			}); err != nil {
				return "", err
			}
			saved++
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", nil
	}
	if !canWrite {
		return "🧾 Li a nota e achei produtos com garantia:\n" + strings.Join(lines, "\n") +
			"\n\nPara eu cadastrar sozinho no Perfil (com aviso de vencimento), ative *IA pode gravar no Perfil* (PROFILE_AI_WRITE). Ou cadastre em Perfil → Casa e bens → Garantia.", nil
	}
	return fmt.Sprintf("🛡️ %d garantia(s) cadastrada(s) no Perfil, com aviso de vencimento:\n%s", saved, strings.Join(lines, "\n")), nil
}

// afterReceipt runs the receipt reader on freshly ingested media and records the outcome.
func (a *Agent) afterReceipt(ctx context.Context, in MediaInput, n *database.Node) {
	if n == nil || !looksLikeReceipt(in.Caption, in.Filename, n) {
		return
	}
	msg, err := a.ReceiptWarranties(ctx, n)
	if err != nil {
		a.log.Warn("nota fiscal: leitura das garantias falhou", "id", n.ID, "err", err)
		return
	}
	if msg == "" {
		return
	}
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta[MetaReceipt] = msg
	_ = a.db.SetMetaKey(ctx, n.ID, MetaReceipt, msg)
}
