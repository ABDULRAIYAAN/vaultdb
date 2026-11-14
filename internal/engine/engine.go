// Package engine runs parsed commands against the store and returns a reply.
//
// The CLI uses it today; the TCP server in a later milestone will reuse it
// unchanged, which is why it returns plain strings instead of printing.
package engine

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ABDULRAIYAAN/vaultdb/internal/parser"
	"github.com/ABDULRAIYAAN/vaultdb/internal/sql"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
	"github.com/ABDULRAIYAAN/vaultdb/internal/vector"
)

// HelpText lists every supported command.
const HelpText = `Commands:
  SET <key> <value>   store a value (use "quotes" for spaces)
  GET <key>           read a value
  DEL <key>           delete a key
  EXISTS <key>        1 if the key exists, 0 if not
  KEYS                list every key, sorted
  SCAN <from> <to> [limit]   keys from <from> up to (not incl.) <to>;
                             use "" for no start or no end
  PREFIX <prefix> [limit]    keys starting with <prefix>
  COUNT               number of keys
  CLEAR               delete everything
  INFO                storage stats: keys, log size, garbage
  COMPACT             rewrite the log with only current data
  HELP                show this list
  EXIT                quit

VECTORS (similarity search, stored as keys under vec:):
  VCREATE <coll> <dim> [COSINE|L2] [HNSW|FLAT]   e.g. VCREATE docs 384
                                        (HNSW: fast, approximate; FLAT: exact)
  VADD <coll> <id> <vector> [text]      VADD docs n1 "[0.1, 0.9, 0.3]" "some text"
  VSEARCH <coll> <vector> [k]           the k nearest vectors (default 5)
  VGET <coll> <id>  |  VDEL <coll> <id>  |  VDROP <coll>  |  VCOLLECTIONS

SQL (tables are stored as keys under sql:):
  CREATE TABLE students (id INT PRIMARY KEY, name TEXT, gpa FLOAT, active BOOL)
  INSERT INTO students VALUES (1, 'Ali', 3.5, TRUE), (2, 'Sara', 3.8, FALSE)
  SELECT name, gpa FROM students WHERE gpa > 3.6 ORDER BY gpa DESC LIMIT 10
  SELECT * FROM students WHERE id IN (1, 2)  |  ... WHERE name LIKE 'A%'
  SELECT COUNT(*), AVG(gpa), MAX(gpa) FROM students WHERE active = TRUE
  SELECT active, COUNT(*), AVG(gpa) FROM students GROUP BY active ORDER BY COUNT(*) DESC
  SELECT active, COUNT(*) FROM students GROUP BY active HAVING AVG(gpa) > 3.5
  SELECT DISTINCT active FROM students  |  SELECT COUNT(DISTINCT gpa) FROM students
  UPDATE students SET gpa = 4.0 WHERE id = 1
  DELETE FROM students WHERE id = 2
  SHOW TABLES  |  DROP TABLE students`

// Engine executes commands.
type Engine struct {
	store *store.Store
	sql   *sql.Executor
	vec   *vector.Manager
}

// New creates an Engine backed by s.
func New(s *store.Store) *Engine {
	return &Engine{store: s, sql: sql.NewExecutor(s), vec: vector.NewManager(s)}
}

var sqlStarts = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	"CREATE": true, "DROP": true, "SHOW": true,
}

// ExecuteLine runs one line typed by the user. Lines that start with a SQL
// word go to the SQL executor; everything else is a key-value command.
func (e *Engine) ExecuteLine(line string) string {
	if first := strings.Fields(line); len(first) > 0 && sqlStarts[strings.ToUpper(first[0])] {
		res, err := e.sql.Exec(line)
		if err != nil {
			return "(error) " + err.Error()
		}
		return sql.FormatTable(res)
	}
	cmd, err := parser.Parse(line)
	if err != nil {
		return "(error) " + err.Error()
	}
	if cmd.Name == "" {
		return ""
	}
	return e.Execute(cmd)
}

// Keys under these prefixes belong to SQL tables and vector collections.
// Changing them with plain SET or DEL would leave those features out of
// step with the store, so it is refused.
var managedPrefixes = []string{"sql:", "vec:"}

func managed(key string) bool {
	for _, p := range managedPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// Execute runs cmd and returns the text to show the user.
func (e *Engine) Execute(cmd parser.Command) string {
	if (cmd.Name == "SET" || cmd.Name == "DEL") && len(cmd.Args) > 0 && managed(cmd.Args[0]) {
		return "(error) keys starting with sql: or vec: are managed by SQL and vector commands; use those instead"
	}
	if strings.HasPrefix(cmd.Name, "V") {
		if reply, ok := e.vectorCommand(cmd); ok {
			return reply
		}
	}
	switch cmd.Name {
	case "SET":
		if len(cmd.Args) != 2 {
			return usage("SET <key> <value>")
		}
		if err := e.store.Set(cmd.Args[0], cmd.Args[1]); err != nil {
			return "(error) " + err.Error()
		}
		return "OK"

	case "GET":
		if len(cmd.Args) != 1 {
			return usage("GET <key>")
		}
		v, err := e.store.Get(cmd.Args[0])
		if err != nil {
			return "(nil)"
		}
		return strconv.Quote(v)

	case "DEL":
		if len(cmd.Args) != 1 {
			return usage("DEL <key>")
		}
		existed, err := e.store.Delete(cmd.Args[0])
		if err != nil {
			return "(error) " + err.Error()
		}
		if existed {
			return "(integer) 1"
		}
		return "(integer) 0"

	case "EXISTS":
		if len(cmd.Args) != 1 {
			return usage("EXISTS <key>")
		}
		if e.store.Exists(cmd.Args[0]) {
			return "(integer) 1"
		}
		return "(integer) 0"

	case "KEYS":
		if len(cmd.Args) != 0 {
			return usage("KEYS")
		}
		keys := e.store.Keys()
		if len(keys) == 0 {
			return "(empty)"
		}
		var b strings.Builder
		for i, k := range keys {
			if i > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "%d) %s", i+1, strconv.Quote(k))
		}
		return b.String()

	case "SCAN":
		if len(cmd.Args) != 2 && len(cmd.Args) != 3 {
			return usage(`SCAN <from> <to> [limit]   (use "" for no start or no end)`)
		}
		limit, err := parseLimit(cmd.Args[2:])
		if err != nil {
			return "(error) " + err.Error()
		}
		return formatPairs(e.store.Scan(cmd.Args[0], cmd.Args[1], limit))

	case "PREFIX":
		if len(cmd.Args) != 1 && len(cmd.Args) != 2 {
			return usage("PREFIX <prefix> [limit]")
		}
		limit, err := parseLimit(cmd.Args[1:])
		if err != nil {
			return "(error) " + err.Error()
		}
		return formatPairs(e.store.Prefix(cmd.Args[0], limit))

	case "COUNT":
		if len(cmd.Args) != 0 {
			return usage("COUNT")
		}
		return fmt.Sprintf("(integer) %d", e.store.Len())

	case "CLEAR":
		if len(cmd.Args) != 0 {
			return usage("CLEAR")
		}
		if err := e.store.Clear(); err != nil {
			return "(error) " + err.Error()
		}
		e.vec.Reset() // collections were just deleted from the store
		return "OK"

	case "INFO":
		if len(cmd.Args) != 0 {
			return usage("INFO")
		}
		st := e.store.Stats()
		if !st.Persistent {
			return fmt.Sprintf("mode: in-memory (nothing saved)\nkeys: %d", st.Keys)
		}
		garbage := 0.0
		if st.LogBytes > 0 {
			garbage = 100 * float64(st.LogBytes-st.LiveBytes) / float64(st.LogBytes)
		}
		auto := "on"
		if !st.AutoCompact {
			auto = "off"
		}
		out := fmt.Sprintf("mode: persistent (append-only log)\nkeys: %d\nlog size: %s\nlive data: %s\ngarbage: %.0f%%\nauto-compaction: %s\ncompactions this session: %d",
			st.Keys, formatBytes(st.LogBytes), formatBytes(st.LiveBytes), garbage, auto, st.Compactions)
		if st.LastCompactErr != nil {
			out += "\nlast compaction error: " + st.LastCompactErr.Error()
		}
		return out

	case "COMPACT":
		if len(cmd.Args) != 0 {
			return usage("COMPACT")
		}
		res, err := e.store.Compact()
		if err != nil {
			return "(error) " + err.Error()
		}
		return fmt.Sprintf("OK, log shrank from %s to %s", formatBytes(res.BytesBefore), formatBytes(res.BytesAfter))

	case "HELP":
		return HelpText

	default:
		return fmt.Sprintf("(error) unknown command %q, type HELP for the list", cmd.Name)
	}
}

// vectorCommand runs V... commands. ok is false if cmd is not one of them.
func (e *Engine) vectorCommand(cmd parser.Command) (reply string, ok bool) {
	a := cmd.Args
	fail := func(err error) (string, bool) { return "(error) " + err.Error(), true }
	switch cmd.Name {
	case "VCREATE":
		if len(a) < 2 || len(a) > 4 {
			return usage("VCREATE <collection> <dimensions> [COSINE|L2] [HNSW|FLAT]"), true
		}
		dim, err := strconv.Atoi(a[1])
		if err != nil {
			return "(error) dimensions must be a whole number", true
		}
		metric := vector.Cosine
		if len(a) >= 3 {
			if metric, err = vector.ParseMetric(a[2]); err != nil {
				return fail(err)
			}
		}
		kind := vector.IndexHNSW
		if len(a) == 4 {
			if kind, err = vector.ParseIndexKind(a[3]); err != nil {
				return fail(err)
			}
		}
		if err := e.vec.Create(a[0], dim, metric, kind); err != nil {
			return fail(err)
		}
		return fmt.Sprintf("OK, collection %s created (%d dimensions, %s, %s index)", a[0], dim, metric, kind), true

	case "VDROP":
		if len(a) != 1 {
			return usage("VDROP <collection>"), true
		}
		n, err := e.vec.Drop(a[0])
		if err != nil {
			return fail(err)
		}
		return fmt.Sprintf("OK, collection %s dropped (%d vectors deleted)", a[0], n), true

	case "VADD":
		if len(a) != 3 && len(a) != 4 {
			return usage(`VADD <collection> <id> "[n1, n2, ...]" ["text"]`), true
		}
		v, err := vector.Parse(a[2])
		if err != nil {
			return fail(err)
		}
		text := ""
		if len(a) == 4 {
			text = a[3]
		}
		isNew, err := e.vec.Add(a[0], a[1], v, text)
		if err != nil {
			return fail(err)
		}
		if isNew {
			return "(integer) 1", true
		}
		return "(integer) 0", true

	case "VGET":
		if len(a) != 2 {
			return usage("VGET <collection> <id>"), true
		}
		v, text, err := e.vec.Get(a[0], a[1])
		if err != nil {
			return fail(err)
		}
		return fmt.Sprintf("vector: %s\ntext: %s", v, strconv.Quote(text)), true

	case "VDEL":
		if len(a) != 2 {
			return usage("VDEL <collection> <id>"), true
		}
		existed, err := e.vec.Delete(a[0], a[1])
		if err != nil {
			return fail(err)
		}
		if existed {
			return "(integer) 1", true
		}
		return "(integer) 0", true

	case "VSEARCH":
		if len(a) != 2 && len(a) != 3 {
			return usage(`VSEARCH <collection> "[n1, n2, ...]" [k]`), true
		}
		q, err := vector.Parse(a[1])
		if err != nil {
			return fail(err)
		}
		k := 5
		if len(a) == 3 {
			if k, err = strconv.Atoi(a[2]); err != nil || k < 1 {
				return "(error) k must be a positive whole number", true
			}
		}
		hits, err := e.vec.Search(a[0], q, k)
		if err != nil {
			return fail(err)
		}
		if len(hits) == 0 {
			return "(empty)", true
		}
		var b strings.Builder
		for i, h := range hits {
			if i > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "%d) %s  distance %.4f", i+1, strconv.Quote(h.ID), h.Distance)
			if h.Payload != "" {
				fmt.Fprintf(&b, "  %s", strconv.Quote(h.Payload))
			}
		}
		return b.String(), true

	case "VCOLLECTIONS":
		list, err := e.vec.List()
		if err != nil {
			return fail(err)
		}
		if len(list) == 0 {
			return "(empty)", true
		}
		var b strings.Builder
		for i, c := range list {
			if i > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "%d) %s  %d dims, %s, %s index, %d vectors", i+1, c.Name, c.Dim, c.Metric, c.Index, c.Count)
		}
		return b.String(), true
	}
	return "", false
}

// DefaultLimit caps SCAN and PREFIX output when no limit is given.
const DefaultLimit = 100

func parseLimit(args []string) (int, error) {
	if len(args) == 0 {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("limit must be a positive whole number, got %q", args[0])
	}
	return n, nil
}

func formatPairs(pairs []store.KV, more bool) string {
	if len(pairs) == 0 {
		return "(empty)"
	}
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d) %s => %s", i+1, strconv.Quote(p.Key), strconv.Quote(p.Value))
	}
	if more {
		fmt.Fprintf(&b, "\n... more results (showing %d; add a bigger limit to see more)", len(pairs))
	}
	return b.String()
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func usage(u string) string {
	return "(error) usage: " + u
}
