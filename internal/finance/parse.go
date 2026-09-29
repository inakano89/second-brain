// Package finance reads bank statements (OFX and CSV), sorts the lines into categories and finds
// recurring charges. Statement lines live in their own table: they never become notes and only
// reach a model as aggregates, under the same policy as the personal profile.
package finance

import (
	"crypto/sha1"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/inakano89/second-brain/internal/database"
)

// ErrNotStatement means the data is neither an OFX file nor a CSV statement.
var ErrNotStatement = errors.New("não parece um extrato bancário (OFX ou CSV com data, descrição e valor)")

func fold(s string) string {
	s = strings.ToLower(norm.NFD.String(s))
	var b strings.Builder
	for _, r := range s {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func hashRef(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Parse reads a statement: OFX when the content looks like it, CSV otherwise. Lines come back
// without category (see Categorizer). warnings mention anything guessed on the way.
func Parse(name string, data []byte) (txs []database.Transaction, warnings []string, err error) {
	head := strings.ToLower(string(data[:min(len(data), 2048)]))
	if strings.Contains(head, "<ofx") || strings.Contains(head, "ofxheader") {
		txs, err = ParseOFX(data)
		return txs, nil, err
	}
	return ParseCSV(name, data)
}

// ---- OFX ----

var ofxTagRe = regexp.MustCompile(`(?i)<([A-Z0-9.]+)>([^<\r\n]*)`)

// ofxBlocks returns the text of each <STMTTRN> (SGML files may leave it unclosed).
func ofxBlocks(text string) []string {
	upper := strings.ToUpper(text)
	var out []string
	for _, part := range strings.Split(upper, "<STMTTRN>")[1:] {
		end := len(part)
		for _, stop := range []string{"</STMTTRN>", "</BANKTRANLIST>", "</CCSTMTRS>", "</STMTRS>"} {
			if i := strings.Index(part, stop); i >= 0 && i < end {
				end = i
			}
		}
		out = append(out, part[:end])
	}
	// upper-case copies only served to find the boundaries: read the values from the original.
	res := make([]string, 0, len(out))
	pos := 0
	for _, o := range out {
		i := strings.Index(upper[pos:], o)
		if i < 0 {
			continue
		}
		res = append(res, text[pos+i:pos+i+len(o)])
		pos += i + len(o)
	}
	return res
}

func ofxFields(block string) map[string]string {
	out := map[string]string{}
	for _, m := range ofxTagRe.FindAllStringSubmatch(block, -1) {
		k := strings.ToUpper(m[1])
		if _, ok := out[k]; !ok {
			out[k] = strings.TrimSpace(m[2])
		}
	}
	return out
}

// ParseOFX reads an OFX (SGML or XML) statement of a bank account or credit card.
func ParseOFX(data []byte) ([]database.Transaction, error) {
	text := string(data)
	account := ""
	if i := strings.Index(strings.ToUpper(text), "<STMTTRN>"); i > 0 {
		f := ofxFields(text[:i])
		account = strings.TrimSpace(f["BANKID"] + "/" + f["ACCTID"])
		if account == "/" {
			account = ""
		}
	}
	var out []database.Transaction
	seen := map[string]int{}
	for _, block := range ofxBlocks(text) {
		f := ofxFields(block)
		date, ok := ofxDate(f["DTPOSTED"])
		amount, aok := parseAmount(f["TRNAMT"], false)
		if !ok || !aok {
			continue
		}
		desc := strings.TrimSpace(f["MEMO"])
		if n := strings.TrimSpace(f["NAME"]); n != "" && !strings.Contains(strings.ToLower(desc), strings.ToLower(n)) {
			desc = strings.TrimSpace(n + " " + desc)
		}
		ref := f["FITID"]
		if ref == "" {
			ref = hashRef(date, strconv.FormatFloat(amount, 'f', 2, 64), desc)
		}
		if n := seen[ref]; n > 0 { // the same id twice in one file is two different lines
			seen[ref]++
			ref += "#" + strconv.Itoa(n)
		} else {
			seen[ref] = 1
		}
		out = append(out, database.Transaction{Date: date, Amount: amount, Description: desc, Account: account, Ref: ref})
	}
	if len(out) == 0 {
		return nil, ErrNotStatement
	}
	return out, nil
}

func ofxDate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 8 {
		return "", false
	}
	t, err := time.Parse("20060102", s[:8])
	if err != nil {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

// ---- CSV ----

type csvCols struct{ date, desc, amount, debit, credit, kind, category, balance int }

func findCols(header []string) (c csvCols, ptHint bool) {
	c = csvCols{-1, -1, -1, -1, -1, -1, -1, -1}
	for i, h := range header {
		k := strings.TrimSpace(fold(strings.TrimPrefix(h, "\ufeff")))
		match := func(words ...string) bool {
			for _, w := range words {
				if k == w || strings.HasPrefix(k, w+" ") || strings.HasPrefix(k, w+"(") {
					return true
				}
			}
			return false
		}
		switch {
		case c.date < 0 && (match("data", "date", "data lancamento", "data movimentacao", "data da compra", "data mov.", "posted date", "transaction date") || strings.HasPrefix(k, "data ")):
			c.date = i
			ptHint = ptHint || strings.HasPrefix(k, "data")
		case c.desc < 0 && match("descricao", "historico", "lancamento", "estabelecimento", "title", "titulo", "memo", "detalhes", "description", "nome", "detalhe", "transacao"):
			c.desc = i
			ptHint = ptHint || k != "title" && k != "memo" && k != "description"
		case c.amount < 0 && match("valor", "amount", "quantia", "montante", "valor (r$)", "value"):
			c.amount = i
			ptHint = ptHint || strings.HasPrefix(k, "valor")
		case c.debit < 0 && match("debito", "saida", "saidas", "debit", "withdrawal"):
			c.debit = i
		case c.credit < 0 && match("credito", "entrada", "entradas", "credit", "deposit"):
			c.credit = i
		case c.kind < 0 && match("tipo", "type", "natureza"):
			c.kind = i
		case c.category < 0 && match("categoria", "category"):
			c.category = i
		case c.balance < 0 && match("saldo", "balance"):
			c.balance = i
		}
	}
	return c, ptHint
}

func (c csvCols) ok() bool {
	return c.date >= 0 && c.desc >= 0 && (c.amount >= 0 || (c.debit >= 0 && c.credit >= 0))
}

func splitCSVHeader(text string) ([]string, rune) {
	text = strings.TrimPrefix(text, "\ufeff")
	comma := sniffComma(text)
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = comma
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	for { // skip the preamble some banks put before the header
		rec, err := r.Read()
		if err != nil {
			return nil, comma
		}
		if c, _ := findCols(rec); c.ok() {
			return rec, comma
		}
		if r.InputOffset() > 4096 {
			return nil, comma
		}
	}
}

func sniffComma(text string) rune {
	best, bestN := ',', -1
	for _, d := range []rune{',', ';', '\t'} {
		n := 0
		for i, ln := range strings.Split(text, "\n") {
			if i > 8 {
				break
			}
			n += strings.Count(ln, string(d))
		}
		if n > bestN {
			best, bestN = d, n
		}
	}
	return best
}

// LooksLikeStatementCSV reports whether the beginning of a CSV file has the columns of a bank
// statement: a date, a description and an amount (or debit and credit).
func LooksLikeStatementCSV(head []byte) bool {
	header, _ := splitCSVHeader(string(head))
	return header != nil
}

// ParseCSV reads a statement exported as CSV (comma, semicolon or tab; Brazilian number and date
// formats; Nubank, Inter, Itaú-style headers). Statements whose values are all positive are read
// as card statements: every line is a charge.
func ParseCSV(name string, data []byte) ([]database.Transaction, []string, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	comma := sniffComma(text)
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = comma
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	var cols csvCols
	var ptHint bool
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil, nil, ErrNotStatement
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: CSV inválido: %w", name, err)
		}
		if c, hint := findCols(rec); c.ok() {
			cols, ptHint = c, hint
			break
		}
		if r.InputOffset() > 4096 {
			return nil, nil, ErrNotStatement
		}
	}
	type row struct {
		date, desc, cat string
		amount          float64
	}
	var rows []row
	pos, neg := 0, 0
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		get := func(i int) string {
			if i >= 0 && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		date, ok := parseDate(get(cols.date))
		if !ok {
			continue // totals, blank lines, footers
		}
		var amount float64
		if cols.amount >= 0 {
			amount, ok = parseAmount(get(cols.amount), ptHint)
			if ok && cols.kind >= 0 { // "Tipo: Débito/Crédito" with an unsigned amount
				k := fold(get(cols.kind))
				if amount > 0 && (strings.HasPrefix(k, "deb") || strings.HasPrefix(k, "saida") || k == "d") {
					amount = -amount
				}
			}
		} else {
			d, dok := parseAmount(get(cols.debit), ptHint)
			c, cok := parseAmount(get(cols.credit), ptHint)
			ok = dok || cok
			amount = c - absF(d)
		}
		if !ok || amount == 0 {
			continue
		}
		rows = append(rows, row{date: date, desc: get(cols.desc), cat: get(cols.category), amount: amount})
		if amount > 0 {
			pos++
		} else {
			neg++
		}
	}
	if len(rows) == 0 {
		return nil, nil, ErrNotStatement
	}
	var warnings []string
	flip := neg == 0 && len(rows) >= 3 && cols.debit < 0 && cols.kind < 0 // all positive: card charges
	if flip {
		warnings = append(warnings, fmt.Sprintf("%s: todos os valores são positivos; tratei como gastos de cartão de crédito.", name))
	}
	base := hashRef(name)
	seen := map[string]int{}
	out := make([]database.Transaction, 0, len(rows))
	for _, x := range rows {
		amount := x.amount
		if flip {
			amount = -amount
		}
		ref := hashRef(x.date, strconv.FormatFloat(amount, 'f', 2, 64), fold(x.desc))
		n := seen[ref]
		seen[ref]++
		if n > 0 {
			ref += "#" + strconv.Itoa(n)
		}
		out = append(out, database.Transaction{Date: x.date, Amount: amount, Description: x.desc, Ref: ref, Account: "csv:" + base[:6]})
	}
	return out, warnings, nil
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

var dateLayouts = []string{"02/01/2006", "2006-01-02", "02-01-2006", "02/01/06", "2006/01/02", "02.01.2006", "2/1/2006", "02/01/2006 15:04:05", "02/01/2006 15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"}

var ptMonths = map[string]time.Month{"jan": 1, "fev": 2, "mar": 3, "abr": 4, "mai": 5, "jun": 6, "jul": 7, "ago": 8, "set": 9, "out": 10, "nov": 11, "dez": 12}

var ptDateRe = regexp.MustCompile(`^(\d{1,2})\s+(?:de\s+)?([a-zç]{3})[a-z]*\.?(?:\s+(?:de\s+)?(\d{4}))?$`)

func parseDate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Format("2006-01-02"), true
		}
	}
	if m := ptDateRe.FindStringSubmatch(fold(s)); m != nil {
		if mon, ok := ptMonths[m[2]]; ok {
			year := time.Now().Year()
			if m[3] != "" {
				year, _ = strconv.Atoi(m[3])
			}
			day, _ := strconv.Atoi(m[1])
			return time.Date(year, mon, day, 0, 0, 0, 0, time.UTC).Format("2006-01-02"), true
		}
	}
	return "", false
}

var amountJunk = regexp.MustCompile(`[^\d.,\-+()]`)

// parseAmount reads "-1.234,56", "R$ 1.234,56", "(123,45)", "1234.56" and "-12.5". ptHint says
// "1.234" means one thousand two hundred thirty-four (Brazilian files) instead of 1.234.
func parseAmount(s string, ptHint bool) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	up := strings.ToUpper(s)
	neg := (strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")")) || strings.HasSuffix(s, "-") || (strings.HasSuffix(up, "D") && strings.ContainsAny(up, "0123456789"))
	s = amountJunk.ReplaceAllString(s, "")
	if strings.HasPrefix(s, "-") {
		neg = true
	}
	s = strings.Trim(s, "()-+")
	if s == "" {
		return 0, false
	}
	comma, dot := strings.LastIndex(s, ","), strings.LastIndex(s, ".")
	switch {
	case comma >= 0 && dot >= 0:
		if comma > dot { // 1.234,56
			s = strings.ReplaceAll(strings.ReplaceAll(s, ".", ""), ",", ".")
		} else { // 1,234.56
			s = strings.ReplaceAll(s, ",", "")
		}
	case comma >= 0:
		s = strings.ReplaceAll(s, ",", ".")
	case dot >= 0 && ptHint && strings.Count(s, ".") >= 1 && len(s)-dot-1 == 3:
		s = strings.ReplaceAll(s, ".", "") // 1.234 → 1234 in a Brazilian file
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}
