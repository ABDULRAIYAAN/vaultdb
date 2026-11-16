// Command sqlite compares VaultDB with SQLite on the same workloads, on the
// same machine, and prints a Markdown table plus a CSV file.
//
// It lives in its own Go module so that VaultDB itself keeps zero
// third-party dependencies. SQLite is the real C library, through
// github.com/mattn/go-sqlite3 (needs a C compiler).
//
//	cd bench/sqlite
//	go run . -out ../../docs/sqlite-comparison.csv
//
// Each workload runs -runs times on a fresh database and the median is
// reported. Both databases write to real files in a temporary folder.
package main

import (
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"

	vsql "github.com/ABDULRAIYAAN/vaultdb/internal/sql"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
	_ "github.com/mattn/go-sqlite3"
)

var (
	runs    = flag.Int("runs", 3, "times to run each workload (median is reported)")
	keys    = flag.Int("keys", 100_000, "keys for the key-value workloads")
	durable = flag.Int("durable", 1_000, "writes for the fsync-on-every-write workload")
	rows    = flag.Int("rows", 20_000, "rows for the SQL workloads")
	out     = flag.String("out", "", "write results as CSV to this file")
)

// result is one line of the comparison table.
type result struct {
	group, name string
	ops         int
	vault, lite time.Duration // median total time for ops operations
}

func main() {
	flag.Parse()
	var res []result
	add := func(group, name string, ops int, v, l func(dir string) time.Duration) {
		r := result{group: group, name: name, ops: ops,
			vault: median(v), lite: median(l)}
		fmt.Fprintf(os.Stderr, "%-34s vaultdb %10s   sqlite %10s\n", name, perOp(r.vault, ops), perOp(r.lite, ops))
		res = append(res, r)
	}

	k := *keys
	add("Key-value", fmt.Sprintf("Write, fsync each (%d)", *durable), *durable,
		func(d string) time.Duration { return vaultWrites(d, *durable, true) },
		func(d string) time.Duration { return liteWrites(d, *durable, "FULL", false) })
	add("Key-value", fmt.Sprintf("Write, no fsync (%dk)", k/1000), k,
		func(d string) time.Duration { return vaultWrites(d, k, false) },
		func(d string) time.Duration { return liteWrites(d, k, "OFF", false) })
	add("Key-value", fmt.Sprintf("Write, one transaction (%dk)", k/1000), k,
		func(d string) time.Duration { return vaultWrites(d, k, false) },
		func(d string) time.Duration { return liteWrites(d, k, "OFF", true) })
	add("Key-value", fmt.Sprintf("Point read (%dk keys)", k/1000), k,
		func(d string) time.Duration { return vaultReads(d, k) },
		func(d string) time.Duration { return liteReads(d, k) })
	add("Key-value", "Range scan, 100 keys (x10k)", 10_000,
		func(d string) time.Duration { return vaultScans(d, k, 10_000) },
		func(d string) time.Duration { return liteScans(d, k, 10_000) })
	add("Key-value", fmt.Sprintf("Reopen with %dk keys", k/1000), 1,
		func(d string) time.Duration { return vaultReopen(d, k) },
		func(d string) time.Duration { return liteReopen(d, k) })

	n := *rows
	for _, w := range sqlWorkloads(n) {
		w := w
		add("SQL", w.name, w.ops,
			func(d string) time.Duration { return vaultSQL(d, n, w) },
			func(d string) time.Duration { return liteSQL(d, n, w) })
	}

	printTable(res)
	if *out != "" {
		if err := writeCSV(*out, res); err != nil {
			fatal(err)
		}
	}
}

// median runs f -runs times, each in a fresh temporary folder.
func median(f func(dir string) time.Duration) time.Duration {
	var ds []time.Duration
	for i := 0; i < *runs; i++ {
		dir, err := os.MkdirTemp("", "vaultdb-bench-*")
		if err != nil {
			fatal(err)
		}
		runtime.GC()
		ds = append(ds, f(dir))
		os.RemoveAll(dir)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func key(i int) string { return fmt.Sprintf("key%08d", i) }

// value is 100 bytes, a typical small record.
func value(i int) string { return fmt.Sprintf("%-100d", i) }

// ---- VaultDB key-value ----

func openVault(dir string, sync bool) *store.Store {
	opts := store.DefaultOptions()
	opts.Sync = sync
	opts.AutoCompact = false
	s, _, err := store.Open(filepath.Join(dir, "vault.log"), opts)
	if err != nil {
		fatal(err)
	}
	return s
}

func fillVault(dir string, n int) *store.Store {
	s := openVault(dir, false)
	for i := 0; i < n; i++ {
		must(s.Set(key(i), value(i)))
	}
	return s
}

func vaultWrites(dir string, n int, sync bool) time.Duration {
	s := openVault(dir, sync)
	defer s.Close()
	start := time.Now()
	for i := 0; i < n; i++ {
		must(s.Set(key(i), value(i)))
	}
	return time.Since(start)
}

func vaultReads(dir string, n int) time.Duration {
	s := fillVault(dir, n)
	defer s.Close()
	order := rand.New(rand.NewSource(1)).Perm(n)
	start := time.Now()
	for _, i := range order {
		v, err := s.Get(key(i))
		if err != nil || len(v) != 100 {
			fatal(fmt.Errorf("vaultdb get %d: %v", i, err))
		}
	}
	return time.Since(start)
}

func vaultScans(dir string, n, scans int) time.Duration {
	s := fillVault(dir, n)
	defer s.Close()
	r := rand.New(rand.NewSource(2))
	start := time.Now()
	for i := 0; i < scans; i++ {
		from := r.Intn(n - 100)
		pairs, _ := s.Scan(key(from), key(from+100), 0)
		if len(pairs) != 100 {
			fatal(fmt.Errorf("vaultdb scan got %d keys", len(pairs)))
		}
	}
	return time.Since(start)
}

func vaultReopen(dir string, n int) time.Duration {
	must(fillVault(dir, n).Close())
	start := time.Now()
	s := openVault(dir, true)
	d := time.Since(start)
	if s.Len() != n {
		fatal(fmt.Errorf("vaultdb reopened with %d keys", s.Len()))
	}
	s.Close()
	return d
}

// ---- SQLite key-value ----

// openLite opens a file database in WAL mode, the usual production setting.
// synchronous FULL fsyncs every commit; OFF leaves it to the OS, like
// VaultDB's -nosync.
func openLite(dir, synchronous string) *sql.DB {
	db, err := sql.Open("sqlite3", filepath.Join(dir, "lite.db"))
	if err != nil {
		fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=" + synchronous} {
		if _, err := db.Exec(p); err != nil {
			fatal(err)
		}
	}
	return db
}

// The key-value table is keyed by the key itself (WITHOUT ROWID), so it is
// one B-tree, the same shape as VaultDB's store.
const kvTable = `CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID`

func liteWrites(dir string, n int, synchronous string, oneTx bool) time.Duration {
	db := openLite(dir, synchronous)
	defer db.Close()
	mustExec(db, kvTable)
	start := time.Now()
	var tx *sql.Tx
	var stmt *sql.Stmt
	var err error
	if oneTx {
		tx, err = db.Begin()
		must(err)
		stmt, err = tx.Prepare(`INSERT OR REPLACE INTO kv VALUES (?, ?)`)
	} else {
		stmt, err = db.Prepare(`INSERT OR REPLACE INTO kv VALUES (?, ?)`)
	}
	must(err)
	for i := 0; i < n; i++ {
		_, err := stmt.Exec(key(i), value(i))
		must(err)
	}
	if tx != nil {
		must(tx.Commit())
	}
	d := time.Since(start)
	stmt.Close()
	return d
}

func fillLite(dir string, n int) *sql.DB {
	db := openLite(dir, "OFF")
	mustExec(db, kvTable)
	tx, err := db.Begin()
	must(err)
	stmt, err := tx.Prepare(`INSERT INTO kv VALUES (?, ?)`)
	must(err)
	for i := 0; i < n; i++ {
		_, err := stmt.Exec(key(i), value(i))
		must(err)
	}
	stmt.Close()
	must(tx.Commit())
	return db
}

func liteReads(dir string, n int) time.Duration {
	db := fillLite(dir, n)
	defer db.Close()
	stmt, err := db.Prepare(`SELECT v FROM kv WHERE k = ?`)
	must(err)
	defer stmt.Close()
	order := rand.New(rand.NewSource(1)).Perm(n)
	var v string
	start := time.Now()
	for _, i := range order {
		if err := stmt.QueryRow(key(i)).Scan(&v); err != nil || len(v) != 100 {
			fatal(fmt.Errorf("sqlite get %d: %v", i, err))
		}
	}
	return time.Since(start)
}

func liteScans(dir string, n, scans int) time.Duration {
	db := fillLite(dir, n)
	defer db.Close()
	stmt, err := db.Prepare(`SELECT k, v FROM kv WHERE k >= ? AND k < ? ORDER BY k`)
	must(err)
	defer stmt.Close()
	r := rand.New(rand.NewSource(2))
	start := time.Now()
	for i := 0; i < scans; i++ {
		from := r.Intn(n - 100)
		rs, err := stmt.Query(key(from), key(from+100))
		must(err)
		got := 0
		var k, v string
		for rs.Next() {
			must(rs.Scan(&k, &v))
			got++
		}
		rs.Close()
		if got != 100 {
			fatal(fmt.Errorf("sqlite scan got %d keys", got))
		}
	}
	return time.Since(start)
}

func liteReopen(dir string, n int) time.Duration {
	must(fillLite(dir, n).Close())
	start := time.Now()
	db := openLite(dir, "FULL")
	var got int
	// SQLite opens lazily, so count the rows to make it actually read them,
	// as VaultDB must when it replays its log.
	must(db.QueryRow(`SELECT COUNT(*) FROM kv`).Scan(&got))
	d := time.Since(start)
	if got != n {
		fatal(fmt.Errorf("sqlite reopened with %d keys", got))
	}
	db.Close()
	return d
}

// ---- SQL ----

// sqlWorkload is a batch of SQL text run against a students table. Both
// databases get the same query strings, parsed every time (no prepared
// statements), because VaultDB's SQL layer has none.
type sqlWorkload struct {
	name    string
	ops     int
	queries func(n int) []string
	check   int // expected rows from the last query, -1 to skip
}

func sqlWorkloads(n int) []sqlWorkload {
	r := rand.New(rand.NewSource(3))
	insert := sqlWorkload{name: fmt.Sprintf("INSERT one row (%dk)", n/1000), ops: n, check: -1}
	return []sqlWorkload{
		insert,
		{name: fmt.Sprintf("SELECT by primary key (%dk)", n/1000), ops: n, check: 1,
			queries: func(n int) []string {
				q := make([]string, n)
				for i := range q {
					q[i] = fmt.Sprintf("SELECT name, gpa FROM students WHERE id = %d", r.Intn(n))
				}
				return q
			}},
		{name: "SELECT pk BETWEEN, 100 rows (x1k)", ops: 1000, check: 100,
			queries: func(n int) []string {
				q := make([]string, 1000)
				for i := range q {
					lo := r.Intn(n - 100)
					q[i] = fmt.Sprintf("SELECT name, gpa FROM students WHERE id BETWEEN %d AND %d", lo, lo+99)
				}
				return q
			}},
		{name: fmt.Sprintf("Full scan with filter, %dk rows (x20)", n/1000), ops: 20, check: -1,
			queries: func(int) []string { return repeat(`SELECT name FROM students WHERE gpa > 3.5 AND dept = 'cs'`, 20) }},
		{name: fmt.Sprintf("GROUP BY with aggregates, %dk rows (x20)", n/1000), ops: 20, check: 4,
			queries: func(int) []string {
				return repeat(`SELECT dept, COUNT(*), AVG(gpa), MAX(gpa) FROM students GROUP BY dept`, 20)
			}},
	}
}

func repeat(q string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = q
	}
	return out
}

var depts = []string{"cs", "math", "physics", "bio"}

func insertRow(i int) string {
	return fmt.Sprintf("INSERT INTO students VALUES (%d, 'student%d', %.2f, '%s')",
		i, i, 2+float64(i%200)/100, depts[i%len(depts)])
}

const studentsTable = `CREATE TABLE students (id INT PRIMARY KEY, name TEXT, gpa FLOAT, dept TEXT)`

func vaultSQL(dir string, n int, w sqlWorkload) time.Duration {
	s := openVault(dir, false)
	defer s.Close()
	e := vsql.NewExecutor(s)
	_, err := e.Exec(studentsTable)
	must(err)
	if w.queries == nil { // the INSERT workload
		start := time.Now()
		for i := 0; i < n; i++ {
			_, err := e.Exec(insertRow(i))
			must(err)
		}
		return time.Since(start)
	}
	for i := 0; i < n; i++ {
		_, err := e.Exec(insertRow(i))
		must(err)
	}
	qs := w.queries(n)
	var last vsql.Result
	start := time.Now()
	for _, q := range qs {
		last, err = e.Exec(q)
		must(err)
	}
	d := time.Since(start)
	if w.check >= 0 && len(last.Rows) != w.check {
		fatal(fmt.Errorf("vaultdb %s: got %d rows, want %d", w.name, len(last.Rows), w.check))
	}
	return d
}

func liteSQL(dir string, n int, w sqlWorkload) time.Duration {
	db := openLite(dir, "OFF")
	defer db.Close()
	// INTEGER PRIMARY KEY makes id SQLite's row key, its fastest layout.
	mustExec(db, `CREATE TABLE students (id INTEGER PRIMARY KEY, name TEXT, gpa FLOAT, dept TEXT)`)
	if w.queries == nil {
		start := time.Now()
		for i := 0; i < n; i++ {
			mustExec(db, insertRow(i))
		}
		return time.Since(start)
	}
	tx, err := db.Begin()
	must(err)
	for i := 0; i < n; i++ {
		_, err := tx.Exec(insertRow(i))
		must(err)
	}
	must(tx.Commit())
	qs := w.queries(n)
	got := 0
	start := time.Now()
	for _, q := range qs {
		rs, err := db.Query(q)
		must(err)
		cols, _ := rs.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		got = 0
		for rs.Next() {
			must(rs.Scan(ptrs...))
			got++
		}
		rs.Close()
	}
	d := time.Since(start)
	if w.check >= 0 && got != w.check {
		fatal(fmt.Errorf("sqlite %s: got %d rows, want %d", w.name, got, w.check))
	}
	return d
}

// ---- output ----

func perOp(d time.Duration, ops int) string {
	ns := float64(d.Nanoseconds()) / float64(ops)
	switch {
	case ns >= 1e6:
		return fmt.Sprintf("%.2f ms", ns/1e6)
	case ns >= 1e3:
		return fmt.Sprintf("%.1f µs", ns/1e3)
	}
	return fmt.Sprintf("%.0f ns", ns)
}

// ratio says how VaultDB compares: "3.2x faster" or "2.1x slower".
func ratio(r result) string {
	v, l := float64(r.vault), float64(r.lite)
	if v <= l {
		return fmt.Sprintf("%.1fx faster", l/v)
	}
	return fmt.Sprintf("%.1fx slower", v/l)
}

func printTable(res []result) {
	var version string
	db, _ := sql.Open("sqlite3", ":memory:")
	db.QueryRow(`SELECT sqlite_version()`).Scan(&version)
	db.Close()
	fmt.Printf("SQLite %s (WAL), %s, %d CPUs, median of %d runs.\n\n", version, runtime.Version(), runtime.NumCPU(), *runs)
	fmt.Println("| Workload | VaultDB | SQLite | VaultDB is |")
	fmt.Println("|---|---|---|---|")
	group := ""
	for _, r := range res {
		if r.group != group {
			group = r.group
			fmt.Printf("| **%s** | | | |\n", group)
		}
		fmt.Printf("| %s | %s | %s | %s |\n", r.name, perOp(r.vault, r.ops), perOp(r.lite, r.ops), ratio(r))
	}
}

func writeCSV(path string, res []result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Write([]string{"group", "workload", "ops", "vaultdb_ns_per_op", "sqlite_ns_per_op"})
	for _, r := range res {
		w.Write([]string{r.group, r.name, strconv.Itoa(r.ops),
			strconv.FormatInt(r.vault.Nanoseconds()/int64(r.ops), 10),
			strconv.FormatInt(r.lite.Nanoseconds()/int64(r.ops), 10)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func mustExec(db *sql.DB, q string) {
	if _, err := db.Exec(q); err != nil {
		fatal(fmt.Errorf("%s: %w", q, err))
	}
}

func must(err error) {
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
