package engine

import (
	"strings"
	"testing"

	"github.com/ABDULRAIYAAN/vaultdb/internal/parser"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

func run(t *testing.T, e *Engine, line string) string {
	t.Helper()
	cmd, err := parser.Parse(line)
	if err != nil {
		t.Fatalf("Parse(%q): %v", line, err)
	}
	return e.Execute(cmd)
}

func TestSession(t *testing.T) {
	e := New(store.New())
	steps := []struct{ in, want string }{
		{`GET name`, `(nil)`},
		{`SET name raiyaan`, `OK`},
		{`GET name`, `"raiyaan"`},
		{`SET bio "CS student at Taylor's"`, `OK`},
		{`GET bio`, `"CS student at Taylor's"`},
		{`EXISTS name`, `(integer) 1`},
		{`COUNT`, `(integer) 2`},
		{`KEYS`, "1) \"bio\"\n2) \"name\""},
		{`DEL name`, `(integer) 1`},
		{`DEL name`, `(integer) 0`},
		{`EXISTS name`, `(integer) 0`},
		{`CLEAR`, `OK`},
		{`KEYS`, `(empty)`},
		{`INFO`, "mode: in-memory (nothing saved)\nkeys: 0"},
		{`COMPACT`, "(error) nothing to compact: the database is in-memory"},
	}
	for _, s := range steps {
		if got := run(t, e, s.in); got != s.want {
			t.Errorf("%s\n got: %s\nwant: %s", s.in, got, s.want)
		}
	}
}

func TestErrors(t *testing.T) {
	e := New(store.New())
	cases := map[string]string{
		`SET onlykey`: `(error) usage: SET <key> <value>`,
		`GET`:         `(error) usage: GET <key>`,
		`SET "" v`:    `(error) key must not be empty`,
		`FLY away`:    `(error) unknown command "FLY", type HELP for the list`,
	}
	for in, want := range cases {
		if got := run(t, e, in); got != want {
			t.Errorf("%s\n got: %s\nwant: %s", in, got, want)
		}
	}
}

func TestPersistentEngine(t *testing.T) {
	path := t.TempDir() + "/vaultdb.log"
	s, _, err := store.Open(path, store.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	e := New(s)
	run(t, e, `SET name raiyaan`)
	run(t, e, `SET x 1`)
	run(t, e, `DEL x`)
	s.Close()

	s2, _, err := store.Open(path, store.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	e2 := New(s2)
	if got := run(t, e2, `GET name`); got != `"raiyaan"` {
		t.Fatalf("GET name after restart = %s", got)
	}
	if got := run(t, e2, `INFO`); !strings.HasPrefix(got, "mode: persistent") {
		t.Fatalf("INFO = %s", got)
	}
	if got := run(t, e2, `COMPACT`); !strings.HasPrefix(got, "OK, log shrank from") {
		t.Fatalf("COMPACT = %s", got)
	}
}

func TestScanAndPrefix(t *testing.T) {
	e := New(store.New())
	for _, kv := range [][2]string{
		{"user:1", "raiyaan"}, {"user:2", "ali"}, {"user:10", "sara"},
		{"users", "x"}, {"order:1", "pizza"}, {"apple", "red"}, {"mango", "yellow"},
	} {
		run(t, e, "SET "+kv[0]+" "+kv[1])
	}
	cases := []struct{ in, want string }{
		{`PREFIX user:`, "1) \"user:1\" => \"raiyaan\"\n2) \"user:10\" => \"sara\"\n3) \"user:2\" => \"ali\""},
		{`PREFIX user: 2`, "1) \"user:1\" => \"raiyaan\"\n2) \"user:10\" => \"sara\"\n... more results (showing 2; add a bigger limit to see more)"},
		{`PREFIX zzz`, `(empty)`},
		{`SCAN a n`, "1) \"apple\" => \"red\"\n2) \"mango\" => \"yellow\""},
		{`SCAN "" b`, "1) \"apple\" => \"red\""},
		{`SCAN users ""`, "1) \"users\" => \"x\""},
		{`SCAN "" "" 1`, "1) \"apple\" => \"red\"\n... more results (showing 1; add a bigger limit to see more)"},
		{`SCAN a`, "(error) usage: SCAN <from> <to> [limit]   (use \"\" for no start or no end)"},
		{`PREFIX user: 0`, `(error) limit must be a positive whole number, got "0"`},
		{`PREFIX user: ten`, `(error) limit must be a positive whole number, got "ten"`},
	}
	for _, c := range cases {
		if got := run(t, e, c.in); got != c.want {
			t.Errorf("%s\n got: %s\nwant: %s", c.in, got, c.want)
		}
	}
}

func TestExecuteLineRoutesSQL(t *testing.T) {
	e := New(store.New())
	steps := []struct{ in, want string }{
		{`create table t (id INT PRIMARY KEY, name TEXT)`, "OK, table t created"},
		{`INSERT INTO t VALUES (1, 'Ali')`, "OK, 1 row inserted"},
		{`SELECT name FROM t`, "+-------+\n| name  |\n+-------+\n| 'Ali' |\n+-------+\n(1 row)"},
		{`SELECT * FROM missing`, "(error) table missing does not exist"},
		{`SET plain value`, "OK"},
		{`GET plain`, `"value"`},
		{`   `, ""},
	}
	for _, s := range steps {
		if got := e.ExecuteLine(s.in); got != s.want {
			t.Errorf("%s\n got: %s\nwant: %s", s.in, got, s.want)
		}
	}
}

func TestVectorCommands(t *testing.T) {
	e := New(store.New())
	steps := []struct{ in, want string }{
		{`VCREATE docs 3`, "OK, collection docs created (3 dimensions, COSINE)"},
		{`VADD docs car "[1, 0, 0]" "The car broke down"`, "(integer) 1"},
		{`VADD docs vehicle "[0.9, 0.1, 0]" "My vehicle stopped working"`, "(integer) 1"},
		{`VADD docs cake "[0, 0, 1]" "I love chocolate cake"`, "(integer) 1"},
		{`VADD docs cake "[0, 0.1, 1]" "I love chocolate cake"`, "(integer) 0"},
		{`VSEARCH docs "[1, 0.05, 0]" 2`, "1) \"car\"  distance 0.0012  \"The car broke down\"\n2) \"vehicle\"  distance 0.0018  \"My vehicle stopped working\""},
		{`VGET docs car`, "vector: [1, 0, 0]\ntext: \"The car broke down\""},
		{`VCOLLECTIONS`, "1) docs  3 dims, COSINE, 3 vectors"},
		{`VDEL docs cake`, "(integer) 1"},
		{`VADD docs x "[1, 2]"`, "(error) collection docs holds 3-dimensional vectors, got 2 numbers"},
		{`VSEARCH nope "[1]"`, "(error) vector collection nope does not exist (create it with VCREATE)"},
		{`VCREATE bad 3 MANHATTAN`, "(error) unknown metric \"MANHATTAN\" (use COSINE or L2)"},
		{`SET vec:item:docs:car hacked`, "(error) keys starting with sql: or vec: are managed by SQL and vector commands; use those instead"},
		{`DEL sql:schema:t`, "(error) keys starting with sql: or vec: are managed by SQL and vector commands; use those instead"},
		{`VDROP docs`, "OK, collection docs dropped (2 vectors deleted)"},
		{`VCREATE docs 2`, "OK, collection docs created (2 dimensions, COSINE)"},
		{`CLEAR`, "OK"},
		{`VCOLLECTIONS`, "(empty)"},
	}
	for _, s := range steps {
		if got := e.ExecuteLine(s.in); got != s.want {
			t.Errorf("%s\n got: %s\nwant: %s", s.in, got, s.want)
		}
	}
}
