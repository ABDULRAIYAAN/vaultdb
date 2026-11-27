// Package engine runs parsed commands against the store and returns a reply.
//
// The CLI uses it today; the TCP server in a later milestone will reuse it
// unchanged, which is why it returns plain strings instead of printing.
package engine

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/parser"
	"github.com/ABDULRAIYAAN/vaultdb/internal/sql"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
	"github.com/ABDULRAIYAAN/vaultdb/internal/vector"
)

// HelpText lists every supported command.
const HelpText = `Commands:
  SET <key> <value>   store a value (use "quotes" for spaces)
      [EX <seconds> | PX <ms> | KEEPTTL]  expire it, or keep its current expiry
      [EXAT <unix-seconds> | PXAT <unix-ms>]  expire it at a set moment
      [NX | XX]                           only if the key is missing / exists
      [GET]                               reply with the old value instead of OK
  GET <key>           read a value
  GETDEL <key>        read a value and delete the key, in one step
  GETEX <key> [EX <seconds> | PX <ms> | EXAT <unix-seconds> | PXAT <unix-ms> | PERSIST]
                      read a value and change its expiry, in one step
  DEL <key>           delete a key
  INCR <key>  |  DECR <key>  add / take away 1 (a missing key starts at 0)
  INCRBY <key> <n>  |  DECRBY <key> <n>   add / take away n
  INCRBYFLOAT <key> <n>  add a decimal number, e.g. 2.5 or -0.75
  APPEND <key> <text>  add text to the end of a value, reply with its length
  SETRANGE <key> <offset> <text>  overwrite bytes from offset on (zero-padded
                      if the value is shorter), reply with its length
  STRLEN <key>        length of a value in bytes (0 if missing)
  GETRANGE <key> <start> <end>  bytes start..end, both included;
                      negative counts from the end, so 0 -1 is all of it
  EXISTS <key>        1 if the key exists, 0 if not
  EXPIRE <key> <seconds>     delete the key after this many seconds
  PEXPIRE <key> <ms>         the same, in milliseconds
  EXPIREAT <key> <unix-seconds>  |  PEXPIREAT <key> <unix-ms>   delete it at a set moment
      [NX | XX | GT | LT]    only if it has no expiry / has one /
                             the new one is later / sooner
  TTL <key>  |  PTTL <key>   time left in seconds / ms (-1: no expiry, -2: no key)
  EXPIRETIME <key>  |  PEXPIRETIME <key>   when it expires, as Unix seconds / ms
  PERSIST <key>              remove the expiry, keep the key
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
  SELECT name FROM students WHERE gpa BETWEEN 3.5 AND 4  |  ... WHERE id NOT BETWEEN 1 AND 2
  SELECT COUNT(*), AVG(gpa), MAX(gpa) FROM students WHERE active = TRUE
  SELECT active, COUNT(*), AVG(gpa) FROM students GROUP BY active ORDER BY COUNT(*) DESC
  SELECT active, COUNT(*) FROM students GROUP BY active HAVING AVG(gpa) > 3.5
  SELECT DISTINCT active FROM students  |  SELECT COUNT(DISTINCT gpa) FROM students
  UPDATE students SET gpa = 4.0 WHERE id = 1
  DELETE FROM students WHERE id = 2
  EXPLAIN SELECT * FROM students WHERE id > 1 AND gpa > 3   (how it runs, without running it)
  CREATE INDEX by_gpa ON students (gpa)   (WHERE on gpa no longer reads the whole table)
  SHOW TABLES  |  DROP TABLE students  |  SHOW INDEXES  |  DROP INDEX by_gpa`

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
	"CREATE": true, "DROP": true, "SHOW": true, "EXPLAIN": true,
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

// writesKey lists the commands that change one key, named by their first
// argument.
var writesKey = map[string]bool{
	"SET": true, "DEL": true, "PERSIST": true, "GETDEL": true,
	"INCR": true, "DECR": true, "INCRBY": true, "DECRBY": true, "INCRBYFLOAT": true,
	"APPEND": true, "SETRANGE": true,
	"EXPIRE": true, "PEXPIRE": true, "EXPIREAT": true, "PEXPIREAT": true,
}

// Execute runs cmd and returns the text to show the user.
func (e *Engine) Execute(cmd parser.Command) string {
	// GETEX only writes when it's given an option; on its own it's a GET.
	writes := writesKey[cmd.Name] || (cmd.Name == "GETEX" && len(cmd.Args) > 1)
	if writes && len(cmd.Args) > 0 && managed(cmd.Args[0]) {
		return "(error) keys starting with sql: or vec: are managed by SQL and vector commands; use those instead"
	}
	if strings.HasPrefix(cmd.Name, "V") {
		if reply, ok := e.vectorCommand(cmd); ok {
			return reply
		}
	}
	switch cmd.Name {
	case "SET":
		if len(cmd.Args) < 2 {
			return usage(setUsage)
		}
		opts, get, err := parseSetOptions(cmd.Args[2:])
		if err != nil {
			return "(error) " + err.Error()
		}
		old, existed, wrote, err := e.store.SetWithOld(cmd.Args[0], cmd.Args[1], opts)
		if err != nil {
			return "(error) " + err.Error()
		}
		if get {
			// Like Redis: the old value whether or not NX/XX let the write
			// through, (nil) if there was none.
			if !existed {
				return "(nil)"
			}
			return strconv.Quote(old)
		}
		if !wrote {
			return "(nil)" // NX or XX said no, like Redis
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

	case "GETDEL":
		if len(cmd.Args) != 1 {
			return usage("GETDEL <key>")
		}
		v, err := e.store.GetDel(cmd.Args[0])
		switch {
		case err == store.ErrNotFound:
			return "(nil)"
		case err != nil:
			return "(error) " + err.Error()
		}
		return strconv.Quote(v)

	case "GETEX":
		if len(cmd.Args) < 1 {
			return usage(getExUsage)
		}
		opts, err := parseGetExOptions(cmd.Args[1:])
		if err != nil {
			return "(error) " + err.Error()
		}
		v, err := e.store.GetEx(cmd.Args[0], opts)
		switch {
		case err == store.ErrNotFound:
			return "(nil)"
		case err != nil:
			return "(error) " + err.Error()
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

	case "INCR", "DECR", "INCRBY", "DECRBY":
		by := cmd.Name == "INCRBY" || cmd.Name == "DECRBY"
		if by && len(cmd.Args) != 2 {
			return usage(cmd.Name + " <key> <n>")
		}
		if !by && len(cmd.Args) != 1 {
			return usage(cmd.Name + " <key>")
		}
		delta := int64(1)
		if by {
			n, err := strconv.ParseInt(cmd.Args[1], 10, 64)
			if err != nil {
				return "(error) the amount must be a whole number that fits in 64 bits"
			}
			delta = n
		}
		if strings.HasPrefix(cmd.Name, "DECR") {
			if delta == math.MinInt64 {
				return "(error) " + store.ErrOverflow.Error() // -delta doesn't fit
			}
			delta = -delta
		}
		n, err := e.store.IncrBy(cmd.Args[0], delta)
		if err != nil {
			return "(error) " + err.Error()
		}
		return fmt.Sprintf("(integer) %d", n)

	case "INCRBYFLOAT":
		if len(cmd.Args) != 2 {
			return usage("INCRBYFLOAT <key> <n>")
		}
		delta, ok := store.ParseDecimal(cmd.Args[1])
		if !ok {
			return "(error) the amount must be a decimal number, like 2.5 or -1e3"
		}
		v, err := e.store.IncrByFloat(cmd.Args[0], delta)
		if err != nil {
			return "(error) " + err.Error()
		}
		return strconv.Quote(v)

	case "APPEND":
		if len(cmd.Args) != 2 {
			return usage("APPEND <key> <text>")
		}
		n, err := e.store.Append(cmd.Args[0], cmd.Args[1])
		if err != nil {
			return "(error) " + err.Error()
		}
		return fmt.Sprintf("(integer) %d", n)

	case "SETRANGE":
		if len(cmd.Args) != 3 {
			return usage("SETRANGE <key> <offset> <text>")
		}
		offset, err := strconv.ParseInt(cmd.Args[1], 10, 64)
		if err != nil {
			return "(error) the offset must be a whole number, like 0 or 6"
		}
		n, err := e.store.SetRange(cmd.Args[0], offset, cmd.Args[2])
		if err != nil {
			return "(error) " + err.Error()
		}
		return fmt.Sprintf("(integer) %d", n)

	case "STRLEN":
		if len(cmd.Args) != 1 {
			return usage("STRLEN <key>")
		}
		return fmt.Sprintf("(integer) %d", e.store.StrLen(cmd.Args[0]))

	case "GETRANGE":
		if len(cmd.Args) != 3 {
			return usage("GETRANGE <key> <start> <end>")
		}
		start, err1 := strconv.ParseInt(cmd.Args[1], 10, 64)
		end, err2 := strconv.ParseInt(cmd.Args[2], 10, 64)
		if err1 != nil || err2 != nil {
			return "(error) start and end must be whole numbers, like 0 and -1"
		}
		return strconv.Quote(e.store.GetRange(cmd.Args[0], start, end))

	case "EXISTS":
		if len(cmd.Args) != 1 {
			return usage("EXISTS <key>")
		}
		if e.store.Exists(cmd.Args[0]) {
			return "(integer) 1"
		}
		return "(integer) 0"

	case "EXPIRE", "PEXPIRE":
		unit, name := time.Second, "seconds"
		if cmd.Name == "PEXPIRE" {
			unit, name = time.Millisecond, "milliseconds"
		}
		if len(cmd.Args) < 2 {
			return usage(cmd.Name + " <key> <" + name + "> " + expireOptionsUsage)
		}
		n, err := strconv.ParseInt(cmd.Args[1], 10, 64)
		if err != nil {
			return "(error) " + name + " must be a whole number"
		}
		opts, err := parseExpireOptions(cmd.Name, cmd.Args[2:])
		if err != nil {
			return "(error) " + err.Error()
		}
		if n > int64(store.MaxTTL/unit) {
			return "(error) " + store.ErrTTLTooLong.Error()
		}
		if n < 0 {
			n = 0 // also deletes; avoids overflow for huge negative numbers
		}
		ok, err := e.store.ExpireWith(cmd.Args[0], time.Duration(n)*unit, opts)
		if err != nil {
			return "(error) " + err.Error()
		}
		return boolInt(ok)

	case "EXPIREAT", "PEXPIREAT":
		unit, name := "unix-seconds", "a Unix time in seconds"
		if cmd.Name == "PEXPIREAT" {
			unit, name = "unix-ms", "a Unix time in milliseconds"
		}
		if len(cmd.Args) < 2 {
			return usage(cmd.Name + " <key> <" + unit + "> " + expireOptionsUsage)
		}
		n, err := strconv.ParseInt(cmd.Args[1], 10, 64)
		if err != nil {
			return "(error) the time must be a whole number (" + name + ")"
		}
		opts, err := parseExpireOptions(cmd.Name, cmd.Args[2:])
		if err != nil {
			return "(error) " + err.Error()
		}
		if n < 0 {
			n = 0 // long past either way: deletes the key
		}
		at, err := unixTime(n, cmd.Name == "EXPIREAT")
		if err != nil {
			return "(error) " + err.Error()
		}
		ok, err := e.store.ExpireAtWith(cmd.Args[0], at, opts)
		if err != nil {
			return "(error) " + err.Error()
		}
		return boolInt(ok)

	case "EXPIRETIME", "PEXPIRETIME":
		if len(cmd.Args) != 1 {
			return usage(cmd.Name + " <key>")
		}
		at, has, err := e.store.Deadline(cmd.Args[0])
		switch {
		case err != nil:
			return "(integer) -2"
		case !has:
			return "(integer) -1"
		case cmd.Name == "PEXPIRETIME":
			return fmt.Sprintf("(integer) %d", at.UnixMilli())
		}
		return fmt.Sprintf("(integer) %d", at.Unix()) // rounded down, like Redis

	case "TTL", "PTTL":
		if len(cmd.Args) != 1 {
			return usage(cmd.Name + " <key>")
		}
		left, has, err := e.store.TTL(cmd.Args[0])
		switch {
		case err != nil:
			return "(integer) -2"
		case !has:
			return "(integer) -1"
		case cmd.Name == "PTTL":
			return fmt.Sprintf("(integer) %d", left.Milliseconds())
		}
		// Rounded up, so a key that is still there never shows 0 seconds.
		return fmt.Sprintf("(integer) %d", (left+time.Second-1)/time.Second)

	case "PERSIST":
		if len(cmd.Args) != 1 {
			return usage("PERSIST <key>")
		}
		ok, err := e.store.Persist(cmd.Args[0])
		if err != nil {
			return "(error) " + err.Error()
		}
		return boolInt(ok)

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
		expiring := ""
		if st.Expiring > 0 {
			expiring = fmt.Sprintf("\nkeys with an expiry: %d", st.Expiring)
		}
		if !st.Persistent {
			return fmt.Sprintf("mode: in-memory (nothing saved)\nkeys: %d", st.Keys) + expiring
		}
		garbage := 0.0
		if st.LogBytes > 0 {
			garbage = 100 * float64(st.LogBytes-st.LiveBytes) / float64(st.LogBytes)
		}
		auto := "on"
		if !st.AutoCompact {
			auto = "off"
		}
		out := fmt.Sprintf("mode: persistent (append-only log)\nkeys: %d%s\nlog size: %s\nlive data: %s\ngarbage: %.0f%%\nauto-compaction: %s\ncompactions this session: %d",
			st.Keys, expiring, formatBytes(st.LogBytes), formatBytes(st.LiveBytes), garbage, auto, st.Compactions)
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

const setUsage = "SET <key> <value> [EX <seconds> | PX <ms> | EXAT <unix-seconds> | PXAT <unix-ms> | KEEPTTL] [NX | XX] [GET]"

// parseSetOptions reads the words after SET's key and value. They can come
// in any order and any case, but each group (EX/PX/EXAT/PXAT/KEEPTTL, NX/XX,
// GET) at most once. get reports whether GET was given.
func parseSetOptions(args []string) (o store.SetOptions, get bool, err error) {
	expiry, cond := "", ""
	for i := 0; i < len(args); i++ {
		word := strings.ToUpper(args[i])
		switch word {
		case "EX", "PX", "EXAT", "PXAT", "KEEPTTL":
			if expiry != "" {
				return o, false, fmt.Errorf("SET takes only one of EX, PX, EXAT, PXAT and KEEPTTL")
			}
			expiry = word
			if word == "KEEPTTL" {
				o.KeepTTL = true
				continue
			}
			if i+1 == len(args) {
				return o, false, fmt.Errorf("%s needs a number of %s", word, expiryUnits[word])
			}
			i++
			if o.TTL, o.At, err = parseExpiry(word, args[i]); err != nil {
				return o, false, err
			}
		case "NX", "XX":
			if cond != "" {
				return o, false, fmt.Errorf("SET takes only one of NX and XX")
			}
			cond = word
			o.IfMissing, o.IfExists = word == "NX", word == "XX"
		case "GET":
			if get {
				return o, false, fmt.Errorf("SET takes GET only once")
			}
			get = true
		default:
			return o, false, fmt.Errorf("unknown SET option %q (expected EX, PX, EXAT, PXAT, KEEPTTL, NX, XX or GET)", args[i])
		}
	}
	return o, get, nil
}

// expiryUnits is what the number after each expiry option counts.
var expiryUnits = map[string]string{
	"EX": "seconds", "PX": "milliseconds",
	"EXAT": "Unix seconds", "PXAT": "Unix milliseconds",
}

// parseExpiry reads the number after EX, PX, EXAT or PXAT (word), for SET
// and GETEX. EX and PX give a duration, EXAT and PXAT a moment.
func parseExpiry(word, arg string) (ttl time.Duration, at time.Time, err error) {
	n, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || n <= 0 {
		return 0, at, fmt.Errorf("%s must be a whole number of %s above zero", word, expiryUnits[word])
	}
	switch word {
	case "EX", "PX":
		unit := time.Second
		if word == "PX" {
			unit = time.Millisecond
		}
		if n > int64(store.MaxTTL/unit) {
			return 0, at, store.ErrTTLTooLong
		}
		return time.Duration(n) * unit, at, nil
	}
	// How far ahead it is gets checked by the store, against its own clock.
	at, err = unixTime(n, word == "EXAT")
	return 0, at, err
}

const getExUsage = "GETEX <key> [EX <seconds> | PX <ms> | EXAT <unix-seconds> | PXAT <unix-ms> | PERSIST]"

// parseGetExOptions reads the words after GETEX's key: at most one of EX,
// PX, EXAT, PXAT (each with its number) and PERSIST, in any case.
func parseGetExOptions(args []string) (o store.GetExOptions, err error) {
	given := false
	for i := 0; i < len(args); i++ {
		word := strings.ToUpper(args[i])
		switch word {
		case "EX", "PX", "EXAT", "PXAT", "PERSIST":
		default:
			return o, fmt.Errorf("unknown GETEX option %q (expected EX, PX, EXAT, PXAT or PERSIST)", args[i])
		}
		if given {
			return o, fmt.Errorf("GETEX takes only one of EX, PX, EXAT, PXAT and PERSIST")
		}
		given = true
		if word == "PERSIST" {
			o.Persist = true
			continue
		}
		if i+1 == len(args) {
			return o, fmt.Errorf("%s needs a number of %s", word, expiryUnits[word])
		}
		i++
		if o.TTL, o.At, err = parseExpiry(word, args[i]); err != nil {
			return o, err
		}
	}
	return o, nil
}

const expireOptionsUsage = "[NX | XX | GT | LT]"

// parseExpireOptions reads the words after EXPIRE's (or PEXPIRE's,
// EXPIREAT's, PEXPIREAT's) key and number: NX, XX, GT and LT in any case,
// each at most once. XX goes with GT or LT, but NX goes with nothing else
// and GT not with LT, the same rules as Redis.
func parseExpireOptions(name string, args []string) (o store.ExpireOptions, err error) {
	seen := map[string]bool{}
	for _, a := range args {
		word := strings.ToUpper(a)
		switch word {
		case "NX", "XX", "GT", "LT":
		default:
			return o, fmt.Errorf("unknown %s option %q (expected NX, XX, GT or LT)", name, a)
		}
		if seen[word] {
			return o, fmt.Errorf("%s takes %s only once", name, word)
		}
		seen[word] = true
	}
	switch {
	case seen["NX"] && (seen["XX"] || seen["GT"] || seen["LT"]):
		return o, fmt.Errorf("NX can't be used with XX, GT or LT")
	case seen["GT"] && seen["LT"]:
		return o, fmt.Errorf("GT and LT can't be used together")
	}
	o.IfNoDeadline, o.IfHasDeadline = seen["NX"], seen["XX"]
	o.IfLater, o.IfSooner = seen["GT"], seen["LT"]
	return o, nil
}

// unixTime turns n Unix seconds (or milliseconds) into a time, refusing
// seconds too large to count in milliseconds. n is not negative.
func unixTime(n int64, seconds bool) (time.Time, error) {
	if !seconds {
		return time.UnixMilli(n), nil
	}
	if n > math.MaxInt64/1000 {
		return time.Time{}, store.ErrTTLTooLong
	}
	return time.UnixMilli(n * 1000), nil
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

func boolInt(b bool) string {
	if b {
		return "(integer) 1"
	}
	return "(integer) 0"
}

func usage(u string) string {
	return "(error) usage: " + u
}
