// Package vector adds similarity search to VaultDB.
//
// An embedding model turns text (or images, or sounds) into a vector: a list
// of numbers that captures its meaning. Things that mean similar things get
// vectors that point in similar directions:
//
//	"The car broke down"          [0.82, 0.10, -0.33, ...]
//	"My vehicle stopped working"  [0.80, 0.12, -0.30, ...]   close
//	"I love chocolate cake"       [-0.45, 0.91, 0.20, ...]   far
//
// So "find things that mean the same" becomes "find the nearest vectors".
// This file holds the vector type and the two ways of measuring distance.
package vector

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Vector is a point in many dimensions. float32 halves the memory of
// float64 and is what embedding models produce anyway.
type Vector []float32

// Metric is how distance between two vectors is measured. Smaller is closer.
type Metric string

const (
	// Cosine compares direction only, ignoring length: 0 means same
	// direction, 1 unrelated, 2 opposite. The usual choice for text.
	Cosine Metric = "COSINE"
	// L2 is ordinary straight-line (Euclidean) distance.
	L2 Metric = "L2"
)

// ParseMetric accepts COSINE or L2 in any case.
func ParseMetric(s string) (Metric, error) {
	switch m := Metric(strings.ToUpper(s)); m {
	case Cosine, L2:
		return m, nil
	}
	return "", fmt.Errorf("unknown metric %q (use COSINE or L2)", s)
}

// Distance returns how far apart a and b are under metric m. Both must have
// the same length.
func Distance(m Metric, a, b Vector) float32 {
	if m == L2 {
		var sum float32
		for i := range a {
			d := a[i] - b[i]
			sum += d * d
		}
		return float32(math.Sqrt(float64(sum)))
	}
	// Cosine distance = 1 - (a·b) / (|a| |b|)
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 1 // a zero vector has no direction: treat it as unrelated
	}
	return 1 - dot/float32(math.Sqrt(float64(na))*math.Sqrt(float64(nb)))
}

// Parse reads a vector written as "[0.1, 0.2, 0.3]" or "0.1,0.2,0.3".
func Parse(s string) (Vector, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("vector is empty")
	}
	parts := strings.Split(s, ",")
	v := make(Vector, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("vector element %d (%q) is not a number", i+1, strings.TrimSpace(p))
		}
		v[i] = float32(f)
	}
	return v, nil
}

// String writes the vector as [0.1, 0.2, 0.3].
func (v Vector) String() string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// encodeItem packs a vector and its text into compact bytes for the store:
//
//	| count (4 bytes) | count x float32 (4 bytes each) | payload text |
//
// Binary takes 4 bytes per number; writing "0.123456789," as text would
// take about 12. With 768-dimension embeddings that's 3 KB instead of 9 KB
// per item.
func encodeItem(v Vector, payload string) string {
	buf := make([]byte, 4+4*len(v)+len(payload))
	binary.LittleEndian.PutUint32(buf, uint32(len(v)))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[4+4*i:], math.Float32bits(f))
	}
	copy(buf[4+4*len(v):], payload)
	return string(buf)
}

func decodeItem(raw string) (Vector, string, error) {
	if len(raw) < 4 {
		return nil, "", errors.New("stored vector is damaged")
	}
	n := int(binary.LittleEndian.Uint32([]byte(raw[:4])))
	if n <= 0 || 4+4*n > len(raw) {
		return nil, "", errors.New("stored vector is damaged")
	}
	b := []byte(raw)
	v := make(Vector, n)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4+4*i:]))
	}
	return v, raw[4+4*n:], nil
}
