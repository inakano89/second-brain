package llm

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// LocalEmbedder is an offline feature-hashing embedder (unigrams + bigrams).
// It gives lexical-semantic similarity with zero dependencies or API cost.
type LocalEmbedder struct{ Dim int }

// EmbedModel implements Embedder.
func (l LocalEmbedder) EmbedModel() string { return "local:hash-512" }

// Embed implements Embedder.
func (l LocalEmbedder) Embed(_ context.Context, texts []string) ([][]float32, Usage, error) {
	dim := l.Dim
	if dim <= 0 {
		dim = 512
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = hashEmbed(t, dim)
	}
	return out, Usage{}, nil
}

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a o e é de da do das dos em no na nos nas um uma uns umas para por com sem que se não nao mais mas
		como ao aos à às ou ser ter foi era está esta este isso isto essa esse ele ela eles elas eu tu você voce nós nos vocês
		meu minha seu sua the of and to in is it for on with as at by an be this that from or are was were not but have has had
		you your we our they their i he she them his her its will can just about into than then there what which who`) {
		m[w] = true
	}
	return m
}()

// Tokenize lowercases, strips diacritics and stopwords.
func Tokenize(s string) []string {
	s = strings.ToLower(norm.NFD.String(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	var out []string
	for _, w := range strings.Fields(b.String()) {
		if len([]rune(w)) < 2 || stopwords[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

func hashEmbed(text string, dim int) []float32 {
	v := make([]float32, dim)
	toks := Tokenize(text)
	add := func(feat string, w float32) {
		h := fnv.New64a()
		h.Write([]byte(feat))
		x := h.Sum64()
		idx := int(x % uint64(dim))
		if (x>>63)&1 == 1 {
			w = -w
		}
		v[idx] += w
	}
	tf := map[string]int{}
	for _, t := range toks {
		tf[t]++
	}
	for t, c := range tf {
		w := float32(1 + math.Log(float64(c)))
		add(t, w)
		if len(t) > 5 { // crude stemming feature
			add("§"+t[:5], 0.5*w)
		}
	}
	for i := 0; i+1 < len(toks); i++ {
		add(toks[i]+"_"+toks[i+1], 0.7)
	}
	var s float64
	for _, x := range v {
		s += float64(x * x)
	}
	if s > 0 {
		inv := float32(1 / math.Sqrt(s))
		for i := range v {
			v[i] *= inv
		}
	}
	return v
}
