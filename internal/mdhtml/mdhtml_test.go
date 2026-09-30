package mdhtml

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	wiki := func(target, label string) string { return "[" + target + "|" + label + "]" }
	got := Render("# Título\n\nTexto **forte** e `código` <b>x</b> [[Nota|rótulo]] https://a.com\n\n- [ ] a\n- [x] b\n\n1. um\n\n> cita\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```\n<raw>\n```", wiki)
	for _, want := range []string{"<h3>Título</h3>", "<strong>forte</strong>", "<code>código</code>", "&lt;b&gt;x&lt;/b&gt;", "[Nota|rótulo]", `href="https://a.com"`, "☐ a", "☑ b", "<ol><li>um</li></ol>", "<blockquote>cita</blockquote>", `<table class="md">`, "&lt;raw&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(Render("[x](javascript:alert(1))", wiki), "<a ") {
		t.Error("javascript: link rendered")
	}
}
