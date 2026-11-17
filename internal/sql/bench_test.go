package sql

import (
	"fmt"
	"testing"

	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

// These benchmarks run the SQL side of bench/sqlite in memory, so the cost of
// the SQL layer itself (parsing, decoding rows, filtering) can be measured
// without a C compiler or a disk in the way.
//
//	go test ./internal/sql -run '^$' -bench . -benchmem

var benchDepts = []string{"cs", "math", "physics", "bio"}

func benchTable(b *testing.B, n int) *Executor {
	b.Helper()
	e := NewExecutor(store.New())
	if _, err := e.Exec(`CREATE TABLE students (id INT PRIMARY KEY, name TEXT, gpa FLOAT, dept TEXT)`); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		q := fmt.Sprintf("INSERT INTO students VALUES (%d, 'student%d', %.2f, '%s')",
			i, i, 2+float64(i%200)/100, benchDepts[i%len(benchDepts)])
		if _, err := e.Exec(q); err != nil {
			b.Fatal(err)
		}
	}
	return e
}

func benchQuery(b *testing.B, e *Executor, q string) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Exec(q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFullScanFilter20k(b *testing.B) {
	benchQuery(b, benchTable(b, 20_000), `SELECT name FROM students WHERE gpa > 3.5 AND dept = 'cs'`)
}

func BenchmarkGroupBy20k(b *testing.B) {
	benchQuery(b, benchTable(b, 20_000), `SELECT dept, COUNT(*), AVG(gpa), MAX(gpa) FROM students GROUP BY dept`)
}

func BenchmarkPKRange100(b *testing.B) {
	benchQuery(b, benchTable(b, 20_000), `SELECT name, gpa FROM students WHERE id BETWEEN 5000 AND 5099`)
}

func BenchmarkPKLookup(b *testing.B) {
	benchQuery(b, benchTable(b, 20_000), `SELECT name, gpa FROM students WHERE id = 12345`)
}

func BenchmarkDecodeRow(b *testing.B) {
	s := &schema{Columns: []column{
		{Name: "id", Type: TypeInt, PK: true}, {Name: "name", Type: TypeText},
		{Name: "gpa", Type: TypeFloat}, {Name: "dept", Type: TypeText},
	}}
	raw, err := encodeRow([]any{int64(12345), "student12345", 3.45, "physics"})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := decodeRow(raw, s); err != nil {
			b.Fatal(err)
		}
	}
}

// The same 100 rows as BenchmarkPKRange100, but asked for with comparisons
// and an extra condition, which the planner turns into the same range walk.
func BenchmarkPKComparisonRange100(b *testing.B) {
	benchQuery(b, benchTable(b, 20_000), `SELECT name, gpa FROM students WHERE id >= 5000 AND id < 5100 AND gpa > 2`)
}
