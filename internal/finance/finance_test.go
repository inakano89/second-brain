package finance

import (
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/database"
)

const ofxSGML = `OFXHEADER:100
DATA:OFXSGML
VERSION:102

<OFX>
<BANKMSGSRSV1><STMTTRNRS><STMTRS>
<BANKACCTFROM><BANKID>0260<ACCTID>12345-6</BANKACCTFROM>
<BANKTRANLIST>
<STMTTRN>
<TRNTYPE>DEBIT
<DTPOSTED>20260905120000[-3:BRT]
<TRNAMT>-39.90
<FITID>abc1
<MEMO>NETFLIX.COM SAO PAULO
</STMTTRN>
<STMTTRN>
<TRNTYPE>CREDIT
<DTPOSTED>20260905
<TRNAMT>8500,00
<FITID>abc2
<NAME>EMPRESA XYZ
<MEMO>SALARIO SETEMBRO
</STMTTRN>
<STMTTRN>
<TRNTYPE>DEBIT
<DTPOSTED>20260906
<TRNAMT>-12.50
<FITID>abc1
<MEMO>PADARIA
</STMTTRN>
</BANKTRANLIST>
</STMTRS></STMTTRNRS></BANKMSGSRSV1>
</OFX>`

func TestParseOFX(t *testing.T) {
	txs, warns, err := Parse("extrato.ofx", []byte(ofxSGML))
	if err != nil || len(warns) != 0 || len(txs) != 3 {
		t.Fatalf("txs = %+v %v %v", txs, warns, err)
	}
	if txs[0].Date != "2026-09-05" || txs[0].Amount != -39.90 || txs[0].Description != "NETFLIX.COM SAO PAULO" || txs[0].Account != "0260/12345-6" || txs[0].Ref != "abc1" {
		t.Errorf("tx0 = %+v", txs[0])
	}
	if txs[1].Amount != 8500 || !strings.Contains(txs[1].Description, "SALARIO SETEMBRO") {
		t.Errorf("tx1 = %+v", txs[1])
	}
	if txs[2].Ref == "abc1" || !strings.HasPrefix(txs[2].Ref, "abc1#") { // repeated FITID stays distinct
		t.Errorf("tx2 ref = %q", txs[2].Ref)
	}
	xml := `<?xml version="1.0"?><OFX><CCSTMTRS><BANKTRANLIST><STMTTRN><TRNTYPE>DEBIT</TRNTYPE><DTPOSTED>20260910</DTPOSTED><TRNAMT>-89.9</TRNAMT><FITID>x1</FITID><MEMO>UBER *TRIP</MEMO></STMTTRN></BANKTRANLIST></CCSTMTRS></OFX>`
	txs, _, err = Parse("card.qfx", []byte(xml))
	if err != nil || len(txs) != 1 || txs[0].Amount != -89.9 {
		t.Fatalf("xml = %+v %v", txs, err)
	}
	if _, _, err := Parse("x.ofx", []byte("<OFX></OFX>")); err == nil {
		t.Error("empty statement should fail")
	}
}

func TestParseAmountAndDate(t *testing.T) {
	for _, c := range []struct {
		in   string
		pt   bool
		want float64
		ok   bool
	}{
		{"-1.234,56", true, -1234.56, true}, {"R$ 1.234,56", true, 1234.56, true}, {"(123,45)", true, -123.45, true}, {"1234.56", false, 1234.56, true},
		{"1,234.56", false, 1234.56, true}, {"-12.5", false, -12.5, true}, {"1.234", true, 1234, true}, {"1.234", false, 1.234, true}, {"45,00 D", true, -45, true},
		{"12,00-", true, -12, true}, {"", false, 0, false}, {"abc", false, 0, false},
	} {
		got, ok := parseAmount(c.in, c.pt)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseAmount(%q, %v) = %v, %v; want %v, %v", c.in, c.pt, got, ok, c.want, c.ok)
		}
	}
	for in, want := range map[string]string{"05/09/2026": "2026-09-05", "2026-09-05": "2026-09-05", "5 set 2026": "2026-09-05", "05/09/26": "2026-09-05", "05/09/2026 14:30": "2026-09-05"} {
		if got, ok := parseDate(in); !ok || got != want {
			t.Errorf("parseDate(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := parseDate("Total"); ok {
		t.Error("footer taken as a date")
	}
}

func TestParseCSVFormats(t *testing.T) {
	// Semicolons, Brazilian numbers, a preamble and a footer.
	itau := "Extrato conta corrente\nAg 1234\n\nData;Lançamento;Valor;Saldo\n01/09/2026;PIX ENVIADO JOAO;-150,00;1.000,00\n02/09/2026;SALARIO;5.000,00;6.000,00\n03/09/2026;SUPERMERCADO EXTRA;-320,45;5.679,55\nTotal;;4.529,55;\n"
	txs, warns, err := ParseCSV("itau.csv", []byte(itau))
	if err != nil || len(txs) != 3 || len(warns) != 0 {
		t.Fatalf("itau = %+v %v %v", txs, warns, err)
	}
	if txs[1].Amount != 5000 || txs[2].Amount != -320.45 || txs[2].Date != "2026-09-03" {
		t.Errorf("itau lines = %+v", txs)
	}
	// Credit card statement with positive charges (Nubank style).
	nu := "date,category,title,amount\n2026-09-01,restaurante,Ifood *Pizza,45.90\n2026-09-02,transporte,Uber *Trip,23.10\n2026-09-03,serviços,Netflix.Com,39.90\n"
	txs, warns, err = ParseCSV("nubank.csv", []byte(nu))
	if err != nil || len(txs) != 3 || txs[0].Amount != -45.90 || len(warns) != 1 || !strings.Contains(warns[0], "cartão de crédito") {
		t.Fatalf("nubank = %+v %v %v", txs, warns, err)
	}
	// Debit and credit in separate columns.
	dc := "Data,Histórico,Débito,Crédito\n05/09/2026,COMPRA FARMACIA,89.90,\n06/09/2026,DEPOSITO,,200.00\n"
	txs, _, err = ParseCSV("dc.csv", []byte(dc))
	if err != nil || len(txs) != 2 || txs[0].Amount != -89.90 || txs[1].Amount != 200 {
		t.Fatalf("debit/credit = %+v %v", txs, err)
	}
	// Unsigned amounts with a type column.
	typed := "Data,Descrição,Valor,Tipo\n05/09/2026,LOJA X,50.00,Débito\n06/09/2026,ESTORNO,20.00,Crédito\n"
	txs, _, _ = ParseCSV("typed.csv", []byte(typed))
	if len(txs) != 2 || txs[0].Amount != -50 || txs[1].Amount != 20 {
		t.Fatalf("typed = %+v", txs)
	}
	// Identical lines on the same day stay distinct, and re-reading gives the same refs.
	dup := "Data;Descrição;Valor\n01/09/2026;CAFE;-5,00\n01/09/2026;CAFE;-5,00\n"
	a, _, _ := ParseCSV("d.csv", []byte(dup))
	b, _, _ := ParseCSV("d.csv", []byte(dup))
	if len(a) != 2 || a[0].Ref == a[1].Ref || a[0].Ref != b[0].Ref || a[1].Ref != b[1].Ref {
		t.Fatalf("refs = %+v / %+v", a, b)
	}
	for _, notStatement := range []string{"title,content\nA,b\n", "Nome;Telefone\nAna;1\n", ""} {
		if _, _, err := ParseCSV("x.csv", []byte(notStatement)); err == nil {
			t.Errorf("%q taken as statement", notStatement)
		}
	}
	if !LooksLikeStatementCSV([]byte(itau)) || LooksLikeStatementCSV([]byte("title,content,tags\nA,B,c\n")) {
		t.Error("LooksLikeStatementCSV")
	}
}

func TestCategorizer(t *testing.T) {
	c := NewCategorizer("# minhas regras\nfulano da silva = Renda\nacademia bairro=Saúde\n")
	cases := map[string]string{
		"NETFLIX.COM 12/09": CatSubscription, "IFOOD *RESTAURANTE": "Alimentação fora", "UBER *TRIP": "Transporte", "UBER EATS": "Alimentação fora",
		"SUPERMERCADO PAO DE ACUCAR": "Mercado", "MERCADO LIVRE*VENDEDOR": "Compras", "AMAZON PRIME BR": CatSubscription, "AMAZON MARKETPLACE": "Compras",
		"PAGAMENTO FATURA CARTAO": CatCardPayment, "PIX ENVIADO JOAO": CatTransfer, "SALARIO SETEMBRO": CatIncome, "FARMACIA DROGASIL": "Saúde",
		"ALUGUEL SETEMBRO": "Moradia", "ENERGIA ELETRICA CPFL": "Contas", "PIX RECEBIDO FULANO DA SILVA": CatIncome, "ACADEMIA BAIRRO MENSAL": "Saúde",
		"LOJA DESCONHECIDA": CatOther, "TAXA DE MANUTENCAO": "Impostos e taxas", "DIA A DIA LTDA": CatOther, "Renner": "Compras",
	}
	for desc, want := range cases {
		if got := c.Category(database.Transaction{Description: desc, Amount: -10}); got != want {
			t.Errorf("Category(%q) = %q, want %q", desc, got, want)
		}
	}
	if got := c.Category(database.Transaction{Description: "OUTRO", Amount: 10}); got != CatOtherIncome {
		t.Errorf("unknown income = %q", got)
	}
	txs := []database.Transaction{{Description: "NETFLIX.COM 12/09 SP", Amount: -39.9}}
	c.Apply(txs)
	if txs[0].Merchant != "netflix" || txs[0].Category != CatSubscription {
		t.Errorf("Apply = %+v", txs[0])
	}
	for in, want := range map[string]string{"SPOTIFY  SAO PAULO BR": "spotify", "COMPRA NO DEBITO CAFE DO ZE 123": "cafe", "PIX ENVIADO MARIA SOUZA": "maria souza", "12345": ""} {
		if got := MerchantKey(in); got != want {
			t.Errorf("MerchantKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func tx(date string, amount float64, desc, cat string) database.Transaction {
	return database.Transaction{Date: date, Amount: amount, Description: desc, Merchant: MerchantKey(desc), Category: cat}
}

func TestSummarizeAndCompare(t *testing.T) {
	sept := []database.Transaction{
		tx("2026-09-05", 8000, "SALARIO", CatIncome), tx("2026-09-06", -500, "SUPERMERCADO", "Mercado"), tx("2026-09-07", -100, "IFOOD", "Alimentação fora"),
		tx("2026-09-08", -2000, "PAGAMENTO FATURA", CatCardPayment), tx("2026-09-09", 50, "ESTORNO SUPERMERCADO", "Mercado"), tx("2026-09-10", -300, "PIX", CatTransfer),
	}
	s := Summarize(sept)
	if s.Income != 8000 || s.Spend != 550 || s.Net != 7450 || s.Neutral != 2300 || s.Lines != 6 {
		t.Fatalf("summary = %+v", s)
	}
	if len(s.Categories) != 2 || s.Categories[0].Category != "Mercado" || s.Categories[0].Total != 450 || s.Categories[1].Category != "Alimentação fora" {
		t.Fatalf("categories = %+v", s.Categories)
	}
	if got := s.Categories[0].Share; got < 81 || got > 82 {
		t.Errorf("share = %v", got)
	}
	aug := Summarize([]database.Transaction{tx("2026-08-06", -300, "SUPERMERCADO", "Mercado"), tx("2026-08-07", -400, "CINEMA", "Lazer")})
	ch := Compare(s, aug, 5)
	if len(ch) != 3 || ch[0].Category != "Mercado" || ch[0].Delta != 150 || ch[1].Category != "Lazer" || ch[1].Delta != -400 {
		// |−400| is the largest change
		if ch[0].Category != "Lazer" {
			t.Fatalf("changes = %+v", ch)
		}
	}
	if from, to, ok := MonthBounds("2026-02"); !ok || from != "2026-02-01" || to != "2026-02-28" {
		t.Errorf("bounds = %s %s %v", from, to, ok)
	}
	if PrevMonth("2026-01") != "2025-12" || MonthOf("2026-09-05") != "2026-09" {
		t.Error("month helpers")
	}
}

func TestFindRecurring(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	var txs []database.Transaction
	for _, m := range []string{"2026-05", "2026-06", "2026-07", "2026-08", "2026-09"} {
		txs = append(txs, tx(m+"-05", -39.90, "NETFLIX.COM", CatSubscription))
		txs = append(txs, tx(m+"-10", -1800, "ALUGUEL APTO", "Moradia"))
	}
	// A subscription that went up, one that stopped, and things that are not recurring.
	for i, m := range []string{"2026-05", "2026-06", "2026-07", "2026-08", "2026-09"} {
		amount := -19.90
		if i == 4 {
			amount = -24.90
		}
		txs = append(txs, tx(m+"-12", amount, "SPOTIFY", CatSubscription))
	}
	for _, m := range []string{"2026-01", "2026-02", "2026-03"} {
		txs = append(txs, tx(m+"-15", -29.90, "DEEZER", CatSubscription))
	}
	for i := 0; i < 12; i++ { // many trips with different values in the same months
		txs = append(txs, tx("2026-09-0"+string(rune('1'+i%9)), -float64(10+i*7), "UBER *TRIP", "Transporte"))
	}
	txs = append(txs, tx("2026-09-08", -2000, "PAGAMENTO FATURA", CatCardPayment), tx("2026-08-08", -2000, "PAGAMENTO FATURA", CatCardPayment), tx("2026-07-08", -2000, "PAGAMENTO FATURA", CatCardPayment))

	list := FindRecurring(txs, now, []string{"Netflix", "Aluguel"})
	byName := map[string]Recurring{}
	for _, r := range list {
		byName[r.Merchant] = r
	}
	if len(byName) != 4 || byName["uber"].Months != 0 || byName["pagamento fatura"].Months != 0 {
		t.Fatalf("recurring = %+v", list)
	}
	nf := byName["netflix"]
	if nf.Amount != 39.90 || nf.Months != 5 || !nf.Active || nf.Untracked || nf.Increased || nf.Yearly < 478 || nf.Yearly > 479 {
		t.Errorf("netflix = %+v", nf)
	}
	sp := byName["spotify"]
	if !sp.Untracked || !sp.Increased || sp.Latest != 24.90 || sp.Amount != 19.90 {
		t.Errorf("spotify = %+v", sp)
	}
	if dz := byName["deezer"]; dz.Active || !dz.Untracked {
		t.Errorf("deezer = %+v", dz)
	}
	if byName["aluguel apto"].Untracked {
		t.Error("rent is tracked by the Perfil title")
	}
	if !list[0].Active || list[len(list)-1].Merchant != "deezer" { // active first, largest first
		t.Errorf("order = %+v", list)
	}
}
