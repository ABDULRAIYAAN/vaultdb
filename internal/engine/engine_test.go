package engine

import (
	"strings"
	"testing"
	"time"

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
		`SET onlykey`: `(error) usage: SET <key> <value> [EX <seconds> | PX <ms> | KEEPTTL] [NX | XX]`,
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
		{`explain SELECT name FROM t WHERE id = 1`, "1. lookup t by primary key: id = 1 (1 key)\n2. filter rows: id = 1"},
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
		{`VCREATE docs 3`, "OK, collection docs created (3 dimensions, COSINE, HNSW index)"},
		{`VADD docs car "[1, 0, 0]" "The car broke down"`, "(integer) 1"},
		{`VADD docs vehicle "[0.9, 0.1, 0]" "My vehicle stopped working"`, "(integer) 1"},
		{`VADD docs cake "[0, 0, 1]" "I love chocolate cake"`, "(integer) 1"},
		{`VADD docs cake "[0, 0.1, 1]" "I love chocolate cake"`, "(integer) 0"},
		{`VSEARCH docs "[1, 0.05, 0]" 2`, "1) \"car\"  distance 0.0012  \"The car broke down\"\n2) \"vehicle\"  distance 0.0018  \"My vehicle stopped working\""},
		{`VGET docs car`, "vector: [1, 0, 0]\ntext: \"The car broke down\""},
		{`VCOLLECTIONS`, "1) docs  3 dims, COSINE, HNSW index, 3 vectors"},
		{`VDEL docs cake`, "(integer) 1"},
		{`VADD docs x "[1, 2]"`, "(error) collection docs holds 3-dimensional vectors, got 2 numbers"},
		{`VSEARCH nope "[1]"`, "(error) vector collection nope does not exist (create it with VCREATE)"},
		{`VCREATE bad 3 MANHATTAN`, "(error) unknown metric \"MANHATTAN\" (use COSINE or L2)"},
		{`SET vec:item:docs:car hacked`, "(error) keys starting with sql: or vec: are managed by SQL and vector commands; use those instead"},
		{`DEL sql:schema:t`, "(error) keys starting with sql: or vec: are managed by SQL and vector commands; use those instead"},
		{`VDROP docs`, "OK, collection docs dropped (2 vectors deleted)"},
		{`VCREATE docs 2 L2 FLAT`, "OK, collection docs created (2 dimensions, L2, FLAT index)"},
		{`VCREATE x 2 L2 TREE`, "(error) unknown index \"TREE\" (use HNSW or FLAT)"},
		{`CLEAR`, "OK"},
		{`VCOLLECTIONS`, "(empty)"},
	}
	for _, s := range steps {
		if got := e.ExecuteLine(s.in); got != s.want {
			t.Errorf("%s\n got: %s\nwant: %s", s.in, got, s.want)
		}
	}
}

func TestExpiryCommands(t *testing.T) {
	e := New(store.New())
	steps := []struct{ in, want string }{
		{`SET session abc`, "OK"},
		{`TTL session`, "(integer) -1"},
		{`TTL missing`, "(integer) -2"},
		{`PTTL missing`, "(integer) -2"},
		{`EXPIRE session 100`, "(integer) 1"},
		{`TTL session`, "(integer) 100"},
		{`INFO`, "mode: in-memory (nothing saved)\nkeys: 1\nkeys with an expiry: 1"},
		{`EXPIRE missing 100`, "(integer) 0"},
		{`PERSIST session`, "(integer) 1"},
		{`PERSIST session`, "(integer) 0"},
		{`TTL session`, "(integer) -1"},
		{`PEXPIRE session 90000`, "(integer) 1"},
		{`TTL session`, "(integer) 90"},
		{`SET session new`, "OK"},
		{`TTL session`, "(integer) -1"},
		{`EXPIRE session 0`, "(integer) 1"},
		{`GET session`, "(nil)"},
		{`SET k v`, "OK"},
		{`EXPIRE k -5`, "(integer) 1"},
		{`EXISTS k`, "(integer) 0"},
		{`EXPIRE k`, "(error) usage: EXPIRE <key> <seconds>"},
		{`PEXPIRE k`, "(error) usage: PEXPIRE <key> <milliseconds>"},
		{`TTL`, "(error) usage: TTL <key>"},
		{`PERSIST`, "(error) usage: PERSIST <key>"},
		{`EXPIRE k soon`, "(error) seconds must be a whole number"},
		{`EXPIRE k 99999999999`, "(error) time to live is too long (the limit is 100 years)"},
		{`EXPIRE sql:schema:t 10`, "(error) keys starting with sql: or vec: are managed by SQL and vector commands; use those instead"},
		{`PERSIST vec:x`, "(error) keys starting with sql: or vec: are managed by SQL and vector commands; use those instead"},
	}
	for _, s := range steps {
		if got := run(t, e, s.in); got != s.want {
			t.Errorf("%s\n got: %s\nwant: %s", s.in, got, s.want)
		}
	}

	// With the real clock: a key given 1 ms is gone a little later.
	run(t, e, `SET blink x`)
	if got := run(t, e, `PEXPIRE blink 1`); got != "(integer) 1" {
		t.Fatalf("PEXPIRE blink 1 = %s", got)
	}
	time.Sleep(20 * time.Millisecond)
	for in, want := range map[string]string{`GET blink`: "(nil)", `TTL blink`: "(integer) -2", `KEYS`: "(empty)"} {
		if got := run(t, e, in); got != want {
			t.Errorf("%s after expiry = %s, want %s", in, got, want)
		}
	}
}

func TestSetOptions(t *testing.T) {
	e := New(store.New())
	const managedErr = "(error) keys starting with sql: or vec: are managed by SQL and vector commands; use those instead"
	steps := []struct{ in, want string }{
		{`SET otp 481516 EX 30`, "OK"},
		{`TTL otp`, "(integer) 30"},
		{`SET otp 123456 KEEPTTL`, "OK"},
		{`TTL otp`, "(integer) 30"},
		{`GET otp`, `"123456"`},
		{`SET otp plain`, "OK"},
		{`TTL otp`, "(integer) -1"},
		{`SET cache v px 90000`, "OK"}, // options are case-insensitive
		{`TTL cache`, "(integer) 90"},
		{`SET lock alice NX EX 10`, "OK"},
		{`SET lock bob EX 10 NX`, "(nil)"}, // any order
		{`GET lock`, `"alice"`},
		{`SET lock carol XX KEEPTTL`, "OK"},
		{`TTL lock`, "(integer) 10"},
		{`SET ghost x XX`, "(nil)"},
		{`EXISTS ghost`, "(integer) 0"},
		{`SET "with space" "a b" ex 5`, "OK"},
		{`TTL "with space"`, "(integer) 5"},

		{`SET k v EX`, "(error) EX needs a number of seconds"},
		{`SET k v PX`, "(error) PX needs a number of milliseconds"},
		{`SET k v EX 0`, "(error) EX must be a whole number of seconds above zero"},
		{`SET k v EX -5`, "(error) EX must be a whole number of seconds above zero"},
		{`SET k v PX soon`, "(error) PX must be a whole number of milliseconds above zero"},
		{`SET k v EX 99999999999`, "(error) time to live is too long (the limit is 100 years)"},
		{`SET k v EX 10 PX 100`, "(error) SET takes only one of EX, PX and KEEPTTL"},
		{`SET k v EX 10 KEEPTTL`, "(error) SET takes only one of EX, PX and KEEPTTL"},
		{`SET k v NX XX`, "(error) SET takes only one of NX and XX"},
		{`SET k v NX NX`, "(error) SET takes only one of NX and XX"},
		{`SET k v GET`, `(error) unknown SET option "GET" (expected EX, PX, KEEPTTL, NX or XX)`},
		{`SET k v 10`, `(error) unknown SET option "10" (expected EX, PX, KEEPTTL, NX or XX)`},
		{`EXISTS k`, "(integer) 0"},
		{`SET sql:row:t:1 x EX 10`, managedErr},
	}
	for _, s := range steps {
		if got := run(t, e, s.in); got != s.want {
			t.Errorf("%s\n got: %s\nwant: %s", s.in, got, s.want)
		}
	}

	// With the real clock: SET ... PX 1 is gone a little later.
	if got := run(t, e, `SET blink x PX 1`); got != "OK" {
		t.Fatalf("SET blink x PX 1 = %s", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := run(t, e, `GET blink`); got != "(nil)" {
		t.Fatalf("GET blink after expiry = %s", got)
	}
	if got := run(t, e, `SET blink y NX`); got != "OK" {
		t.Fatalf("SET NX on an expired key = %s, want OK", got)
	}
}
