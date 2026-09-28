package extract

import (
	"strings"
	"testing"
)

func TestReadability(t *testing.T) {
	doc := `<html><head><title>T</title><meta property="og:title" content="Título OG"></head><body>
<nav><a href="/">Home</a><a href="/x">Menu</a></nav>
<div class="sidebar"><p>Assine nossa newsletter, promoções, ofertas, cupons.</p></div>
<div class="post-content">
<h1>Manchete</h1>
<p>Primeiro parágrafo do artigo com bastante texto, vírgulas, detalhes e contexto relevante para a leitura.</p>
<p>Segundo parágrafo, também longo o suficiente, com mais informações, dados e <a href="https://fonte.com">uma fonte</a>.</p>
<ul><li>item um</li><li>item dois</li></ul>
</div><footer>rodapé</footer><script>alert(1)</script></body></html>`
	a, err := Readability(doc)
	if err != nil {
		t.Fatal(err)
	}
	if a.Title != "Título OG" || !strings.Contains(a.Text, "Primeiro parágrafo") || strings.Contains(a.Text, "alert") || strings.Contains(a.Text, "rodapé") {
		t.Fatalf("article = %+v", a)
	}
	if !strings.Contains(a.Text, "[uma fonte](https://fonte.com)") || !strings.Contains(a.Text, "- item um") {
		t.Fatalf("markdown conversion: %q", a.Text)
	}
}
