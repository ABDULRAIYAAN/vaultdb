package ai

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// ---- Local hashing embedder

// HashEmbedder needs no model and no internet. It splits text into words,
// and each word (and each pair of neighbouring words) adds 1 to one of dim
// buckets chosen by a hash of the word. Texts sharing many words end up with
// similar vectors. It understands no meaning ("car" and "vehicle" are
// unrelated to it), but it is a working, instant way to try the pipeline.
type HashEmbedder struct{ dim int }

// NewHashEmbedder creates a hashing embedder with dim dimensions.
func NewHashEmbedder(dim int) *HashEmbedder { return &HashEmbedder{dim: dim} }

func (h *HashEmbedder) Name() string { return fmt.Sprintf("local/hash-%d", h.dim) }

func (h *HashEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, h.dim)
		words := tokenize(t)
		add := func(s string, w float32) {
			f := fnv.New32a()
			f.Write([]byte(s))
			sum := f.Sum32()
			sign := float32(1)
			if sum&1 == 1 { // a second "hash bit" decides the sign, reducing collisions' effect
				sign = -1
			}
			v[int(sum>>1)%h.dim] += sign * w
		}
		for j, w := range words {
			add(w, 1)
			if j > 0 {
				add(words[j-1]+" "+w, 0.5)
			}
		}
		var norm float64
		for _, f := range v {
			norm += float64(f * f)
		}
		if norm > 0 {
			inv := float32(1 / math.Sqrt(norm))
			for k := range v {
				v[k] *= inv
			}
		}
		out[i] = v
	}
	return out, nil
}

var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true, "to": true, "in": true,
	"is": true, "are": true, "was": true, "it": true, "for": true, "on": true, "with": true, "as": true,
	"this": true, "that": true, "be": true, "by": true, "at": true, "what": true, "how": true, "does": true,
	"do": true, "i": true, "my": true, "me": true, "about": true, "which": true, "from": true,
}

func tokenize(s string) []string {
	var words []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if !stopWords[w] {
			words = append(words, w)
		}
	}
	return words
}
