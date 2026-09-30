package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inakano89/second-brain/internal/database"
)

const testOFX = `OFXHEADER:100
DATA:OFXSGML

<OFX><BANKMSGSRSV1><STMTTRNRS><STMTRS><BANKACCTFROM><BANKID>1<ACCTID>99</BANKACCTFROM><BANKTRANLIST>
<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20260905<TRNAMT>-39.90<FITID>1<MEMO>NETFLIX.COM</STMTTRN>
<STMTTRN><TRNTYPE>CREDIT<DTPOSTED>20260905<TRNAMT>5000.00<FITID>2<MEMO>SALARIO</STMTTRN>
<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20260906<TRNAMT>-120.00<FITID>3<MEMO>SUPERMERCADO EXTRA</STMTTRN>
</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>`

func TestImportStatement(t *testing.T) {
	im, db, cfg, dir := setupImporter(t)
	ctx := context.Background()
	cfg.Update(map[string]string{"FINANCE_RULES": "netflix=Streaming"})
	ofx := filepath.Join(dir, "extrato.ofx")
	os.WriteFile(ofx, []byte(testOFX), 0o600)
	csv := filepath.Join(dir, "cartao.csv")
	os.WriteFile(csv, []byte("date,category,title,amount\n2026-09-01,restaurante,Ifood *Pizza,45.90\n2026-09-02,transporte,Uber *Trip,23.10\n2026-09-03,x,Spotify,21.90\n"), 0o600)

	rep := im.Run(ctx, []File{{Path: ofx, Name: "extrato.ofx"}, {Path: csv, Name: "cartao.csv"}}, Options{})
	if rep.State != StateDone || rep.Failed != 0 || rep.Lines != 6 || rep.LinesDup != 0 || rep.Created != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.Contains(rep.FormatList(), "extrato (6)") {
		t.Errorf("formats = %s", rep.FormatList())
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "cartão de crédito") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	if n, _ := db.CountNodes(ctx, database.NodeFilter{}); n != 0 {
		t.Fatalf("statement lines became %d notes", n)
	}
	txs, _ := db.Transactions(ctx, "", "", 0)
	cats := map[string]string{}
	for _, x := range txs {
		cats[x.Description] = x.Category
		if x.Batch != rep.ID {
			t.Errorf("batch not set: %+v", x)
		}
	}
	if cats["NETFLIX.COM"] != "Streaming" || cats["SALARIO"] != "Renda" || cats["SUPERMERCADO EXTRA"] != "Mercado" || cats["Ifood *Pizza"] != "Alimentação fora" {
		t.Fatalf("categories = %v", cats)
	}
	// Importing the same files again adds nothing.
	again := im.Run(ctx, []File{{Path: ofx, Name: "extrato.ofx"}, {Path: csv, Name: "cartao.csv"}}, Options{})
	if again.Lines != 0 || again.LinesDup != 6 {
		t.Fatalf("re-import = %+v", again)
	}
	if n, _ := db.CountTransactions(ctx); n != 6 {
		t.Fatalf("stored = %d", n)
	}
	// A CSV of notes with no statement columns is still a normal import.
	notes := filepath.Join(dir, "notas.csv")
	os.WriteFile(notes, []byte("title,content\nA,um\nB,dois\n"), 0o600)
	if rep := im.Run(ctx, []File{{Path: notes, Name: "notas.csv"}}, Options{}); rep.Created != 2 || rep.Lines != 0 {
		t.Fatalf("notes csv = %+v", rep)
	}
	// An OFX inside a zip is found too (two of its lines were imported already).
	zp := filepath.Join(dir, "banco.zip")
	os.WriteFile(zp, zipBytes(t, map[string]string{"itau/jan.ofx": strings.Replace(testOFX, "<FITID>1<", "<FITID>zip1<", 1)}), 0o600)
	if rep := im.Run(ctx, []File{{Path: zp, Name: "banco.zip"}}, Options{}); rep.Lines != 1 || rep.LinesDup != 2 {
		t.Fatalf("zip = %+v", rep)
	}
}
