package finance

import (
	"strings"
	"unicode"

	"github.com/inakano89/second-brain/internal/database"
)

// Category names.
const (
	CatIncome       = "Renda"
	CatTransfer     = "Transferências"
	CatCardPayment  = "Pagamento de fatura"
	CatOther        = "Outros"
	CatOtherIncome  = "Outras entradas"
	CatSubscription = "Assinaturas"
)

// neutral categories move money around without being income or spending.
var neutral = map[string]bool{CatTransfer: true, CatCardPayment: true, "Investimentos": true}

// IsNeutral reports whether a category is left out of income and spending totals.
func IsNeutral(cat string) bool { return neutral[cat] }

type rule struct {
	cat   string
	words []string
}

// builtin rules, in priority order: the first match wins. Short words match whole words only.
var builtin = []rule{
	{CatCardPayment, []string{"pagamento fatura", "pgto fatura", "pagamento de fatura", "fatura cartao", "pagto fatura", "pag fatura"}},
	{CatIncome, []string{"salario", "folha de pagamento", "pagamento de salario", "13o salario", "decimo terceiro", "rendimento", "dividendos", "proventos", "pro labore", "prolabore"}},
	{"Investimentos", []string{"aplicacao", "resgate", "tesouro direto", "cdb", "lci", "lca", "corretora", "xp investimentos", "rico investimentos", "b3", "fundo de investimento"}},
	{"Alimentação fora", []string{"ifood", "rappi", "uber eats", "ubereats", "restaurante", "lanchonete", "pizzaria", "hamburgueria", "burger", "mcdonalds", "mc donalds", "subway", "starbucks", "cafeteria", "padaria", "sorveteria", "churrascaria", "bar", "pizza", "sushi", "outback", "habibs", "spoleto"}},
	{CatSubscription, []string{"netflix", "spotify", "amazon prime", "prime video", "disney", "hbo", "youtube premium", "youtubepremium", "deezer", "apple.com/bill", "apple com bill", "icloud", "google one", "google storage", "chatgpt", "openai", "microsoft 365", "office 365", "adobe", "dropbox", "globoplay", "paramount", "crunchyroll", "telecine", "premiere", "duolingo", "notion", "github", "canva", "kindle unlimited", "audible", "tidal", "twitch", "patreon", "strava"}},
	{"Compras", []string{"mercado livre", "mercadolivre"}},
	{"Mercado", []string{"supermercado", "mercado", "carrefour", "pao de acucar", "atacadao", "assai", "big bompreco", "sacolao", "hortifruti", "zaffari", "atacarejo", "mambo", "st marche", "savegnago"}},
	{"Transporte", []string{"uber", "99", "99pop", "cabify", "posto", "shell", "ipiranga", "petrobras", "combustivel", "estacionamento", "pedagio", "sem parar", "semparar", "conectcar", "metro", "bilhete unico", "onibus", "cptm", "sptrans", "gasolina", "etanol", "zona azul", "lava jato", "oficina", "mecanica"}},
	{"Saúde", []string{"farmacia", "drogaria", "droga raia", "drogasil", "pacheco", "pague menos", "panvel", "unimed", "amil", "bradesco saude", "sulamerica", "hospital", "clinica", "laboratorio", "dentista", "odonto", "fleury", "dasa", "medico", "consulta", "psicologo", "fisioterapia"}},
	{"Moradia", []string{"aluguel", "condominio", "iptu", "imobiliaria", "financiamento habitacional", "leroy merlin", "telhanorte", "c&c"}},
	{"Contas", []string{"energia", "enel", "cpfl", "light", "cemig", "copel", "sabesp", "copasa", "sanepar", "agua", "gas", "comgas", "naturgy", "internet", "vivo", "claro", "tim", "oi fibra", "net", "telefone", "celular", "algar", "brisanet", "energisa"}},
	{"Educação", []string{"escola", "faculdade", "universidade", "curso", "udemy", "alura", "coursera", "mensalidade", "colegio", "idiomas", "wizard", "fisk", "cel lep", "livraria", "saraiva"}},
	{"Viagens", []string{"hotel", "airbnb", "booking", "latam", "gol linhas", "azul linhas", "passagem", "decolar", "hurb", "cvc", "pousada", "hostel", "aeroporto", "localiza", "movida", "rentcars"}},
	{"Lazer", []string{"cinema", "ingresso", "teatro", "show", "steam", "playstation", "xbox", "nintendo", "livelo", "sympla", "eventim", "ticketmaster", "parque", "boliche", "clube", "academia", "smartfit", "smart fit", "bluefit", "bodytech"}},
	{"Compras", []string{"amazon", "mercado livre", "mercadolivre", "shopee", "aliexpress", "magalu", "magazine luiza", "americanas", "shein", "renner", "zara", "c&a", "riachuelo", "casas bahia", "ponto frio", "fast shop", "kabum", "netshoes", "centauro", "dafiti", "havaianas", "natura", "boticario", "sephora", "ikea", "tok stok", "leroy"}},
	{"Impostos e taxas", []string{"imposto", "tarifa", "iof", "anuidade", "juros", "multa", "taxa", "irpf", "darf", "das", "ipva", "licenciamento", "seguro", "cesta de servicos", "pacote de servicos"}},
	{CatTransfer, []string{"pix", "ted", "doc", "transferencia", "transf", "tev", "saque"}},
}

// Categorizer sorts statement lines into categories using the user's rules first.
type Categorizer struct {
	user []rule
}

// NewCategorizer parses the user's rules: one "palavra=Categoria" per line (# starts a comment).
func NewCategorizer(userRules string) *Categorizer {
	c := &Categorizer{}
	for _, ln := range strings.Split(userRules, "\n") {
		if i := strings.Index(ln, "#"); i >= 0 {
			ln = ln[:i]
		}
		word, cat, ok := strings.Cut(ln, "=")
		word, cat = fold(strings.TrimSpace(word)), strings.TrimSpace(cat)
		if ok && word != "" && cat != "" {
			c.user = append(c.user, rule{cat: cat, words: []string{word}})
		}
	}
	return c
}

func tokens(folded string) []string {
	return strings.FieldsFunc(folded, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func matches(folded string, toks []string, word string) bool {
	if strings.ContainsAny(word, " /.&") {
		return strings.Contains(folded, word)
	}
	for _, t := range toks {
		if t == word || (len(word) >= 5 && strings.HasPrefix(t, word)) {
			return true
		}
	}
	return false
}

// Category returns the category of one line.
func (c *Categorizer) Category(t database.Transaction) string {
	folded := fold(t.Description)
	toks := tokens(folded)
	for _, list := range [][]rule{c.user, builtin} {
		for _, r := range list {
			for _, w := range r.words {
				if matches(folded, toks, w) {
					// A positive "Compras"/"Mercado" line is a refund, not spending: keep the category.
					return r.cat
				}
			}
		}
	}
	if t.Amount > 0 {
		return CatOtherIncome
	}
	return CatOther
}

// Apply fills Merchant and Category of every line.
func (c *Categorizer) Apply(txs []database.Transaction) {
	for i := range txs {
		txs[i].Merchant = MerchantKey(txs[i].Description)
		txs[i].Category = c.Category(txs[i])
	}
}

var merchantNoise = map[string]bool{
	"compra": true, "debito": true, "credito": true, "pix": true, "pgto": true, "pagto": true, "pagamento": true, "transf": true, "ted": true, "doc": true,
	"br": true, "brasil": true, "sp": true, "sao": true, "paulo": true, "rj": true, "ltda": true, "sa": true, "eireli": true, "me": true, "epp": true, "de": true, "da": true, "do": true,
	"em": true, "no": true, "na": true, "cartao": true, "parcela": true, "parc": true, "recorrente": true, "com": true, "www": true, "cnpj": true, "enviado": true, "recebido": true,
}

// MerchantKey reduces a statement description to the name of the merchant ("NETFLIX.COM 12/09 SP"
// → "netflix"), so the same charge always groups together.
func MerchantKey(desc string) string {
	var keep []string
	for _, t := range tokens(fold(desc)) {
		if len(t) < 3 || merchantNoise[t] || isNumber(t) {
			continue
		}
		keep = append(keep, t)
		if len(keep) == 2 {
			break
		}
	}
	return strings.Join(keep, " ")
}

func isNumber(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}
