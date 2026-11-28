# Dev log

Short notes on what changed and why, newest last.

## 2026-11-10: SQL aggregate functions

Added `COUNT(col)`, `SUM`, `AVG`, `MIN` and `MAX` next to the existing `COUNT(*)`, and a select list can now hold several of them at once (`SELECT COUNT(*), AVG(gpa), MAX(gpa) FROM students`).

Why: until now the only question you could ask about a whole table was "how many rows?". Averages and highest/lowest values are the first thing anyone tries after `SELECT *`, and they're a stepping stone to `GROUP BY`.

How it works:

- The AST's `Count bool` became `Aggregates []Aggregate`, each one a function name plus a column (empty for `*`).
- Only `COUNT` is a keyword. The parser treats `SUM`, `AVG`, `MIN` and `MAX` as functions only when a `(` follows, so existing tables with a column called `max` or `sum` keep working.
- The executor walks the matching rows once per aggregate, skipping `NULL`s like standard SQL. `SUM` on an `INT` column keeps an exact `int64` total and returns an error on overflow instead of wrapping around; `AVG` adds up in floating point.
- Mixing plain columns with aggregates is rejected with a clear message until `GROUP BY` exists, and so is `ORDER BY` (there's only one result row).

One behaviour change: `LIMIT` used to cut the rows *before* `COUNT(*)` counted them, so `SELECT COUNT(*) FROM t LIMIT 2` said 2 even with 100 rows. It now applies to the result row, as in Postgres and SQLite.

## 2026-11-11: GROUP BY

Added `GROUP BY` to `SELECT`, so aggregates can be split per group: `SELECT dept, COUNT(*), AVG(gpa) FROM students GROUP BY dept`. `ORDER BY` can now sort by an aggregate too (`ORDER BY COUNT(*) DESC`), which is how you get "top N" style answers.

Why: yesterday's aggregates could only summarise a whole table. "How many per department?" or "which category has the highest average?" are the questions people actually ask, and you couldn't answer them without running one query per value.

How it works:

- The AST's separate `Columns` and `Aggregates` lists became a single `Items []SelectItem`, because a select list can now mix both and the result has to keep the order they were written in.
- The parser checks the one rule that needs no schema: a plain column next to an aggregate must be in `GROUP BY`. It also rejects `SELECT *` with `GROUP BY` and a column listed twice.
- The executor buckets the matching rows in a map keyed by the JSON text of their `GROUP BY` values (each column has one type, so equal values always give equal text). NULLs land in the same bucket, as in standard SQL. Groups are then sorted by their keys so the output is deterministic, re-sorted if there's an `ORDER BY`, and cut by `LIMIT`.
- Column and type checks for aggregates moved into `checkAggregate`, which runs before any rows are touched, so a typo is reported even when no rows (and so no groups) match.
- An aggregate query without `GROUP BY` is just the one-group case of the same code, so it still always returns exactly one row.

`GROUP` is now a reserved word, so a column can't be called `group` any more. Next up is `HAVING`, to filter groups after they're aggregated.

## 2026-11-12: HAVING

Added `HAVING` to `SELECT`, so grouped results can be filtered by their aggregates: `SELECT dept, COUNT(*) FROM students GROUP BY dept HAVING COUNT(*) >= 2`. It works without `GROUP BY` too, where the whole table is one group.

Why: `GROUP BY` answered "how many per department?", but not "which departments have more than N?" or "which ones average above 3.5?". `WHERE` can't do it because it runs on single rows before anything is counted, so this needed its own clause.

How it works:

- New expression node `AggregateRef`, an aggregate used inside a condition. The parser only builds one while it's reading a `HAVING` (a flag on the parser), so `WHERE COUNT(*) > 1` or `UPDATE t SET a = SUM(b)` now fail with a message pointing to `HAVING` instead of a vague "expected a value".
- The parser also checks that every plain column in `HAVING` is a `GROUP BY` column, the same rule as the select list, and rejects `HAVING` on a query with neither `GROUP BY` nor aggregates (that's just `WHERE`).
- In the executor, `HAVING` runs right after grouping and before `ORDER BY`/`LIMIT`. For each group, `bindGroup` rewrites the condition into one with only literals in it: each aggregate becomes its value over the group's rows, each `GROUP BY` column becomes the group's value. That goes through the normal `eval`, so `HAVING` gets exactly the same NULL and comparison rules as `WHERE` with no second evaluator to keep in sync.
- Aggregates in `HAVING` are type-checked up front with `checkAggregate`, so `HAVING SUM(name) > 1` is an error even when no rows match.

`HAVING` is now a reserved word. Next on the SQL side is probably `DISTINCT` or simple `JOIN`s.

## 2026-11-13: DISTINCT

Added `SELECT DISTINCT` and `DISTINCT` inside aggregates: `SELECT DISTINCT dept FROM students` lists each department once, and `SELECT COUNT(DISTINCT dept) FROM students` says how many there are.

Why: "which values are there?" and "how many different ones?" are two of the most common questions after `COUNT(*)`. `GROUP BY` could already fake the first one, but not the second, and `COUNT(DISTINCT user)` (unique visitors, unique customers) is something you can't get any other way without pulling every row out.

How it works:

- `Select` got a `Distinct` flag and `Aggregate` got one too, so `COUNT(DISTINCT dept)` is its own aggregate with its own heading (`count(distinct dept)`). `DISTINCT` is now a reserved word. `COUNT(DISTINCT *)` is rejected with a hint to name a column.
- The executor builds the result rows as before, then `distinctRows` keeps the first copy of each one using a map keyed by the JSON text of the row, so `ORDER BY` order survives. `LIMIT` now runs after that step, otherwise `SELECT DISTINCT dept ... LIMIT 2` could return the same department twice and then stop. It works the same way on grouped results.
- For aggregates, `aggregate()` keeps a set of values it has already used and skips repeats. Values are always `int64`, `float64`, `string` or `bool`, so they work directly as Go map keys.
- The parser refuses `ORDER BY` on a column that a `SELECT DISTINCT` doesn't show, like Postgres does. Once duplicates are merged there's no single hidden value left to sort by.

While testing I found that `0.0` and `-0.0` landed in different `GROUP BY` groups because their JSON text differs (`0` vs `-0`). Grouping and `DISTINCT` now share one `valuesKey` helper that turns `-0.0` into `0.0` first, with a test for both.

Next on the SQL side: simple `JOIN`s, or `IN (...)` and `LIKE` in conditions.

## 2026-11-14: IN and LIKE

Added `IN (...)` and `LIKE` to conditions, each with a `NOT` form: `WHERE id IN (1, 3, 7)`, `WHERE name LIKE 'A%'`, `WHERE dept NOT IN ('eng', 'ops')`. They work anywhere a condition does, so `UPDATE`, `DELETE` and `HAVING` get them too.

Why: until now "is it one of these values?" meant a long chain of `OR`s, and there was no way at all to search text by prefix or pattern. Both are things you reach for in the first five minutes with any SQL database.

How it works:

- Two new AST nodes, `In` and `Like`. `IN` and `LIKE` are now reserved words. The parser handles them at the comparison level, next to `IS NULL`, and a `NOT` straight after a value can only mean `NOT IN` or `NOT LIKE`, so `a NOT = 1` gets a clear error. `IN ()` with nothing inside is rejected.
- `IN` compares with the same `compare` that `=` uses, and follows the SQL NULL rules: a match is TRUE, no match is FALSE, but no match when the list has a `NULL` is NULL. That means `x NOT IN (1, NULL)` never keeps a row, which surprises everyone the first time, but it's what Postgres and SQLite do, so I matched it and wrote it down in the README.
- `LIKE` compiles the pattern into tokens (literal, `_`, `%`, with `\` as the escape) and then runs the two-pointer glob matcher: remember the last `%`, and on a mismatch go back there and let it eat one more character. That's at most text × pattern steps, so a pattern like `'%a%a%a%a%b'` can't blow up the way a naive recursive matcher does. It works on runes, so `_` matches `é` as one character. It's case-sensitive, like Postgres.
- The primary key shortcut grew: `WHERE id IN (3, 1, 7)` now does one key lookup per value instead of scanning the table. Keys are sorted and de-duplicated so rows come back in the same order a scan would give. If any value can't be turned into a key exactly (like `1.0` for an `INT` key) it falls back to the scan, so the shortcut can never change an answer.
- The four places that walk expressions (`checkColumns`, `checkHavingColumns`, `havingAggregates`, `bindGroup`) learned the new nodes, which is how `HAVING COUNT(*) IN (1, 3)` works with no extra code in the evaluator.

Tests: parser and executor cases for both, including the `NOT IN` + `NULL` trap and the pk lookup fallback, plus a fuzz test (`FuzzLikeMatch`) that checks the matcher against Go's `regexp` package on random text and patterns. It ran clean for about 600k inputs.

Next on the SQL side: `BETWEEN`, or simple `JOIN`s.

## 2026-11-15: BETWEEN

Added `BETWEEN` and `NOT BETWEEN` to conditions: `WHERE gpa BETWEEN 3.5 AND 4`, `WHERE id NOT BETWEEN 2 AND 3`, `HAVING COUNT(*) BETWEEN 2 AND 5`. Both ends are included.

Why: ranges are one of the most common filters there is (dates, prices, scores, id ranges), and writing `x >= a AND x <= b` every time is noisy. It was also a chance to make range queries on the primary key fast, which nothing used yet even though the B-tree has supported range walks since v0.4.0.

How it works:

- New AST node `Between` (value, low end, high end, `Negate`), and `BETWEEN` is now a reserved word. The parser reads it at the comparison level next to `IN` and `LIKE`. The ends are read as plain values, not whole conditions, so the `AND` inside `BETWEEN` can never be confused with a normal `AND`: `a BETWEEN 1 AND 5 AND b = 2` is `(a BETWEEN 1 AND 5) AND b = 2`, same as Postgres.
- I gave it its own node and `evalBetween` instead of rewriting it into `x >= a AND x <= b` in the parser. The reason is NULLs: the standard says `x BETWEEN a AND b` is the two comparisons joined with three-valued `AND` (FALSE beats NULL, NULL beats TRUE), so `9 BETWEEN NULL AND 5` is FALSE and `9 NOT BETWEEN NULL AND 5` is TRUE. `evalBetween` does exactly that. All three values are evaluated first so a type mistake is reported even when one end already decides the answer.
- Primary key shortcut: `pkRange` spots `WHERE pk BETWEEN literal AND literal` and turns it into one `Scan(from, to)` over the B-tree, instead of reading every row. `to` is the high key plus a `\x00` byte, the smallest string that sorts after it, which turns the store's half-open `[from, to)` into an inclusive range. It only does this for `INT` keys (fixed-width hex with the sign bit flipped, so key order is number order) and `TEXT` keys (the text itself). `FLOAT` keys, mixed types like `id BETWEEN 0.5 AND 2.5`, and `NOT BETWEEN` fall back to the normal scan, so the shortcut can never change an answer. The executor's `Store` interface gained `Scan` for this.
- The four expression walkers (`checkColumns`, `checkHavingColumns`, `havingAggregates`, `bindGroup`) learned the new node, so `HAVING` works with no extra evaluator code.

Tests: parser cases for precedence and errors, executor cases for ends, NULL rules, columns as ends, `HAVING`, `UPDATE`/`DELETE`, and a `TEXT` primary key next to a table with a similar name (so a range can't leak into the neighbouring table's keys). `TestBetweenMatchesComparisons` runs 300 random ranges, including the int64 extremes and reversed ends, against both an `INT` and a `TEXT` key and checks the shortcut gives the same rows as `>= AND <=` while never doing a full table read. I also checked it catches a real bug: dropping the `\x00` makes six tests fail.

One thing I noticed while writing the NULL cases: plain `AND`/`OR` in `eval` treat NULL like FALSE (`l == true && r == true`), so `NOT (x > 1 AND y > 1)` with `y` NULL comes out TRUE when standard SQL says NULL. `BETWEEN` doesn't go through that path, so it's correct, but fixing `AND`/`OR` to proper three-valued logic is next.

## 2026-11-16: v1.0.0

VaultDB is now 1.0. This release was about proving the work rather than adding features: real tests where there were none, a comparison against a real database, and fixing the one wrong answer I already knew about.

- **Three-valued `AND` and `OR`.** Last time I noted that `AND`/`OR` treated `NULL` like `FALSE`, so `NOT (x > 1 AND y > 1)` with `y` missing came out `TRUE` and `WHERE` kept a row it shouldn't. They now follow the SQL truth tables in one small `logic` function: the value that settles the answer on its own wins (`FALSE` for `AND`, `TRUE` for `OR`), otherwise any `NULL` makes the result `NULL`. Non-boolean operands are now an error instead of silently counting as `FALSE`. `TestLogicTruthTable` checks all 18 combinations, and `TestThreeValuedLogic` checks it through `WHERE`. I put the old code back temporarily to make sure both tests fail on it, and they do.
- **Client tests.** `internal/client` had no tests of its own (the server tests used it, but nothing checked how it handles a bad server). A fake server now sends it replies with newlines inside, empty replies, broken headers, negative and oversized lengths, and bodies that are cut off, and 20 goroutines share one client to check every command gets its own reply. Coverage went from 0% to 100%.
- **VaultDB vs SQLite.** `bench/sqlite` runs the same workloads on both, with real files, and reports the median of 3 runs. VaultDB wins every key-value test except reopening (7.7x faster point reads, 1.4x faster durable writes), because all keys live in memory and nothing goes through a C call. SQLite wins every SQL test, by 11 to 40x on scans and grouping, because VaultDB decodes a JSON row for every row it touches and has no planner. Both sides of that are in the README, along with what makes each number fair. It is a separate Go module, so the database itself still has zero dependencies.
- **CI.** A GitHub Actions workflow runs `go vet` and the whole suite under the race detector on every push.

Numbers at 1.0: about 8,100 lines of Go and 3,700 lines of tests, 127 tests plus a fuzz test, 89.7% coverage of `internal/`.

Next: a binary row format and a simple query planner, to close the SQL gap with SQLite.

## 2026-11-16: Binary row format

SQL rows are no longer stored as JSON. Each row is now a small binary record: a format byte (`0x01`), the number of values, then for each value a one-byte type tag and its data (`INT` as a zig-zag varint, `FLOAT` as its 8 IEEE 754 bytes, `TEXT` as a length and the bytes, `NULL`/`TRUE`/`FALSE` as just the tag). The code is in `internal/sql/row.go`.

Why: the SQLite comparison showed full scans 40x slower than SQLite, and a quick profile-by-benchmark showed why: decoding one JSON row took 1.4 µs, and a scan of 20k rows took 38 ms, so about three quarters of every scan was just `encoding/json`. The roadmap's next step was "binary row format and a simple query planner", and this is the first half.

How it works:

- `encodeRow` appends tags and values into one buffer sized up front. `decodeRow` looks at the first byte: `0x01` means binary, anything else goes to the old JSON decoder. JSON rows always start with `[`, so the two can never be confused. That means databases written by v1.0.0 open as-is with no migration step, and an old row gets rewritten in binary the next time an `UPDATE` touches it. A test mixes both formats in one table to check this.
- The decoder reads straight from the stored string with a tiny `rowReader` instead of converting to `[]byte`, which would copy every row, and text values are substrings of the row, so they cost no copy either. It went from 19 allocations per row to 5.
- It's strict: the value count must match the schema, every tag must match its column's type (`NULL` fits anywhere), varints can't run past 64 bits, text lengths can't run past the end, and there can't be bytes left over. A damaged row gives "stored row is damaged" instead of a wrong answer.
- Side fix: JSON can only hold valid UTF-8, so the old format silently turned bad bytes in `TEXT` into U+FFFD. Binary stores the bytes exactly. `TestTextIsStoredExactly` checks this across a restart.

Numbers (in memory, 20k rows, median of 3, new `internal/sql/bench_test.go`): decoding one row 1.41 µs to 0.14 µs, full scan with filter 38.4 ms to 7.6 ms (5x), `GROUP BY` 52.4 ms to 15.2 ms (3.5x), 100-row primary key range 212 µs to 51 µs (4.2x), point lookup 9.0 µs to 6.3 µs. Rows aren't smaller on disk, though: a float takes 8 bytes where JSON might write `3.9`, so it's about a wash. The goal was decode speed, not size.

Tests: round trips for every type including the int64 extremes, `-0.0`, `NaN`, infinities, empty and 300-byte strings and invalid UTF-8; a byte-by-byte layout test so the format can't change by accident; every truncated prefix of a valid row must be rejected; a list of hand-made damaged rows; and `FuzzDecodeRow`, which checks random bytes never panic and anything accepted survives another encode/decode. It ran clean for about 585k inputs. I also flipped the sign in the zig-zag decode on purpose and 12 tests failed, so the tests do catch a broken decoder.

The SQLite comparison table in the README is still the v1.0.0 run; I'll re-run it with the C compiler setup before the next release.

Next: a simple query planner, the other half of roadmap item 10.

## 2026-11-17: A query planner and EXPLAIN

The other half of roadmap item 10. Until now the executor had three hard-coded shortcuts, each matching one exact shape of `WHERE`: `pk = x`, `pk IN (...)` and `pk BETWEEN a AND b`. Anything else read the whole table, even `WHERE id >= 100 AND id < 200`, or `WHERE id = 5 AND active = TRUE`, where the primary key condition is right there. I replaced the shortcuts with a small planner in `internal/sql/plan.go`, and added `EXPLAIN` so you can see what it picked.

Why: this is the most common way real queries miss the fast path. A range written with comparisons, or a key lookup with one extra filter, are both normal SQL, and both were paying for a full scan. It's also the base a secondary index will need later: something has to decide which rows to read before the executor reads them.

How it works:

- `planAccess` splits `WHERE` at its top-level `AND`s. `WHERE` only keeps rows where the whole thing is TRUE, and `a AND b` is only TRUE when both are, so each piece has to hold on its own and can narrow the read by itself. `OR` and `NOT` are left whole, since neither side of an `OR` has to hold.
- Each piece about the primary key becomes exact keys (`=`, `IN`), a lower or upper bound (`>`, `>=`, `<`, `<=`, either way round, and `BETWEEN`), or "nothing can match" (any comparison with `NULL`). Then they're intersected: the tightest bound on each side, and only listed keys that fall inside the bounds. Bounds that cross, like `id > 5 AND id < 5`, read nothing at all.
- Bounds are stored as keys with the open or closed end already worked in: `> v` starts at `key(v) + "\x00"` (the smallest key after it) and `<= v` ends there, so a tighter bound is just a bigger or smaller string. That only works because key order is value order, which was already true for `INT` and `TEXT` keys, the only types a primary key can have. With no lower or upper bound, the range is clamped to `sql:row:<table>:` up to `sql:row:<table>;` (`;` is the byte after `:`), so a range can never run into another table's rows.
- Every row the plan reads is still checked against the full `WHERE`. So the planner only needs to be safe, never exact: reading too much costs time, skipping a row would be a bug. Anything it can't turn into keys exactly (`id > 2.5` on an `INT` key, `id = 'one'`) is just left to that check.
- `EXPLAIN SELECT/UPDATE/DELETE` checks the table and every column name, then prints numbered steps: how rows are read, the filter, then grouping, `HAVING`, sorting, `DISTINCT`, `LIMIT`. It never runs the statement. To print the filter I wrote `exprString`, which turns an expression tree back into SQL with brackets wherever `AND` meets `OR`. It returns plain text lines rather than a result table, because a table would put every step in quotes like a `TEXT` value.

Tests: a table of 30-odd plans with their `EXPLAIN` lines; `TestPlannerMatchesFullScan`, which runs 2,000 random `AND` chains (mixed `=`, `<`, `>=`, `!=`, `IN`, `BETWEEN`, `NULL`, literal-first comparisons, `OR`s and non-key columns) on an `INT` table and a `TEXT` table with similarly named neighbours, and checks each against the same query wrapped in `NOT NOT (...)`, which the planner can't see into and so always reads everything; checks that planned `UPDATE` and `DELETE` never read the whole table; and a round trip that every printed expression parses back into the same tree. I also broke the planner four ways on purpose (no `\x00` on an open lower bound, not flipping `5 < id`, not dropping keys outside the bounds, a sloppy table prefix) and each one fails at least one test. The old `BETWEEN` test was comparing against `>= AND <=`, which now goes through the planner too, so it now uses `NOT NOT` as well.

Numbers: on 20,000 rows, `WHERE id >= 5000 AND id < 5100 AND gpa > 2` went from 10.1 ms to about 75 µs, the same as the `BETWEEN` version. New benchmark: `BenchmarkPKComparisonRange100`.

One thing to note: because fewer rows are read, a type error in another part of `WHERE` (say `name > 5`) now only shows up if one of the rows the plan reads hits it. That was already true for `pk = x`, and it's how row-by-row checks work anywhere, but the real fix is checking types before running, which I'd like to do at some point.

Next: secondary indexes (`CREATE INDEX`), so the planner can use columns other than the primary key.

## 2026-11-18: Secondary indexes

Roadmap item 10c. Until now the primary key was the only column that could skip a full table read: `WHERE name = 'Ali'` read all 20,000 rows to find one. `CREATE INDEX by_name ON students (name)` now gives any column the same shortcut, and the planner from yesterday picks it up on its own. Also `DROP INDEX` and `SHOW INDEXES`. The code is in `internal/sql/index.go`.

Why: the planner was built so a secondary index could slot in, and this was the obvious gap. Looking up rows by something other than their id is most of what real queries do.

How it works:

- An index is one extra key per row: `sql:idx:<table>:<index>:` then the column's value, an end marker, and the row's own key, pointing at the row. Because the store is a B-tree, all entries for one value sit together, so `name = 'Ali'` is a short range walk over the index plus a GET per row it finds.
- Values are encoded so key order is value order. `INT` is the same flipped-sign hex as primary keys. `FLOAT` uses the standard trick on the IEEE 754 bits: set the sign bit for positive numbers, flip every bit for negative ones, and `-0.0` is turned into `0.0` first because SQL says they're equal. `TEXT` is its bytes, but with `0x00` written as `0x00 0xFF`, and every value ends with `0x00 0x01`. Without the escape, the entries for `'a'` would be a prefix of the entries for `'a\x00b'` and a lookup would pick up both. With it, a value's entries are exactly the keys from value+`00 01` to value+`00 02`.
- The planner didn't need a second copy of its logic. I pulled out a `keySpace`: for a column, where a value's keys start, where they end, and the range the whole space covers. The primary key is one key space and each index is another; the same code turns the `AND`-ed conditions into exact keys, bounds or "nothing matches" for each one, and the best plan wins (nothing > exact values > a range > full read, primary key on ties). It's a fixed rule, not statistics, so it can pick a worse index than it could, but never a plan worse than a full read.
- Index plans collect row keys from the entries, sort them and GET each row, so results come back in primary key order like every other plan, and as always every row is checked against the full `WHERE`.
- Writes keep the index in step, in an order that can only leave extra entries if the process dies halfway: `INSERT` writes the entry before the row, `UPDATE` writes the new entry, then the row, then removes the old one, `DELETE` removes the row before the entry. `CREATE INDEX` builds every entry before adding the index to the schema, and `DROP INDEX` takes it out of the schema before deleting entries. An extra entry costs a wasted GET; a missing one would make a query silently skip a row.
- `NULL`s aren't indexed: no comparison with `NULL` is TRUE, so no lookup could want them.
- `INDEX`, `INDEXES` and `ON` are matched as plain words only where they're expected, not added as keywords, so an existing table with a column called `index` or `on` doesn't suddenly stop parsing.

Tests: parser cases; a lifecycle test that runs lookups, ranges and grouping through indexes after inserts, updates (including one planned by the very index it changes) and deletes, and checks nothing reads the whole table; `TestIndexValueOrder`, which checks that for ~300 values of each type, including the int64 and float extremes, infinities, `-0.0` and text full of `00`, `01` and `FF` bytes, key order matches `compare` and each value's range holds only its own entries; `TestIndexMatchesFullScan`, 1,500 random rounds of `INSERT`/`UPDATE`/`DELETE` and a random query on a table with an index on an `INT`, `FLOAT`, `TEXT` and `BOOL` column, comparing every result against a full read and checking the indexes match the rows exactly; a restart test with a real log file; and `TestIndexCrashLeavesNoGaps`, which cuts `INSERT`, `UPDATE` and `DELETE` off after every possible number of writes and checks no row is ever missing from its index or from a query. I broke it seven ways on purpose (no entry on insert, no text escaping, no float flip, no `-0` fix, removing the old entry before writing the row, removing entries an update still needs, leaving entries after delete) and each one fails at least one test.

Numbers (20,000 rows, in memory): `WHERE name = 'student12345'` 7.5 ms without an index, 10 µs with one. 100 rows by `gpa = 3.45` takes about 80 µs and 500 rows by `gpa >= 3.95` about 440 µs. The cost is on writes: an `INSERT` with two indexes is 13 µs instead of 9 µs.

Not done yet: indexes over more than one column, `IS NULL` through an index, and `ORDER BY` reading straight from an index instead of sorting. The playground site still runs the WebAssembly build from before, so it doesn't have `CREATE INDEX` until I rebuild it.

## 2026-11-19: Expiring keys

The key-value side can now forget things on a timer: `EXPIRE key seconds`, `PEXPIRE key ms`, `TTL`/`PTTL` to see what's left (`-1` for no expiry, `-2` for no key, like Redis), and `PERSIST` to take the expiry off again. `INFO` shows how many keys have one. The store code is in `internal/store/ttl.go`.

Why: sessions, caches, rate-limit counters and one-time codes are what people actually put in a key-value store, and every one of them needs to disappear by itself. Without it you'd need a cron job deleting keys, and nothing stops a read from seeing a stale value in between.

How it works:

- The log has a new record type, `EXPIRE` (op 4). Its value is the **deadline** in Unix milliseconds, not "60 seconds", and an empty value means `PERSIST`. Saving the absolute time is what makes it survive a restart properly: if the database is down for 20 seconds, a key with a minute left comes back with 40 seconds, not a fresh minute, and a key whose time ran out while it was closed is dropped as it opens. The format version stays at 2, since old logs never contain op 4; a version 1 log that somehow does is treated as damaged.
- The store keeps a `map[key]deadline` next to the B-tree. From the deadline on, every read skips the key: `GET`, `EXISTS`, `KEYS`, `SCAN`/`PREFIX` (whose limit only counts live keys) and `COUNT`. So it's gone at exactly that millisecond as far as anyone can tell.
- Actually freeing the memory is lazy, the way Redis does it, because reads only hold the read lock and can't change anything. An expired key is removed when a write touches it, after every write (which checks up to 20 keys with a deadline, and since Go starts every map walk at a random spot, they all get checked over time), at compaction and at startup. None of that writes a `DEL` record. The log already says when the key runs out, so replaying it always gets the same answer.
- The reaping after a write happens *after* the change is applied, not before. Otherwise an `EXPIRE` could check the key is alive, then the reaper drops it a millisecond later, and the `EXPIRE` record lands in the log for a key memory no longer has, which would bring it back to life on restart.
- A plain `SET` clears the deadline (Redis does the same), `EXPIRE` with zero or a negative number deletes straight away, and anything under a millisecond rounds up to one. `TTL` rounds up too, so a key that is still there never shows `0`.
- Compaction writes a `SET` and then an `EXPIRE` for each key with a deadline, and `liveBytes` counts both, so the garbage percentage in `INFO` stays right.
- Keys under `sql:` and `vec:` can't be given an expiry, same as `SET` and `DEL`. A row disappearing on its own would leave its index entries pointing at nothing.

Tests: the deadline boundary to the millisecond across every read path; `Scan`'s limit not being used up by expired keys; `SET` and `PERSIST` clearing deadlines; writes reaping 500 untouched expired keys and `liveBytes` coming back to exactly one key's worth; a restart test where the database is closed for 20 seconds; and `TestExpiryMatchesModel`, which runs 4,000 random `SET`/`DEL`/`EXPIRE`/`PERSIST` steps, clock jumps, compactions and restarts against a plain map with a fake clock, and checks after every compaction that the log is exactly as big as `liveBytes` predicted. I broke it nine ways on purpose (no reaping at startup, `SET` not clearing the deadline, compaction dropping deadlines, not subtracting the `EXPIRE` record from `liveBytes`, `<` instead of `<=` at the deadline, `SCAN` and `PREFIX` not skipping expired keys, no reaping after writes, rounding short durations down) and each one fails at least one test.

Not done yet: `SET key value EX seconds` in one command. Doing it as two log records would let a crash in between leave the key with no expiry, so it wants a combined record, which I'll do separately. The playground still runs the old WebAssembly build, so it doesn't have these commands until I rebuild it.

## 2026-11-20: SET options (EX, PX, KEEPTTL, NX, XX)

Yesterday's leftover. `SET key value EX 60` (or `PX` in milliseconds) now stores a value and its expiry in one step, `KEEPTTL` replaces a value without touching its expiry, and `NX` / `XX` only write if the key is missing / already there, replying `(nil)` when they don't. They combine in any order, so `SET lock worker-1 NX EX 30` is a lock that only one client gets and that frees itself if that client dies. The store side is `SetWith` in `internal/store/ttl.go`.

Why: with only `SET` then `EXPIRE`, the two are separate log records. If the power goes between them, the value is on disk and the deadline isn't, so a one-time code or a lock never goes away. That's the worst kind of bug for a lock, and it's the reason I didn't just make `SET ... EX` call both.

How it works:

- New log record type, `SETEX` (op 5). Its value is the deadline as 8 big-endian bytes of Unix milliseconds, then the value. It has one checksum like every record, so after a crash it's either all there or cut off and dropped on the next start. Format version stays 2, same reasoning as `EXPIRE`: older logs never contain op 5, and a version 1 log that does is treated as damaged.
- Replay handles `SETEX` as a `SET` followed by giving the key its deadline, so everything downstream (reads skipping expired keys, reaping, `TTL`) didn't need to change.
- `NX` and `XX` are checked under the write lock, and an expired key that's still in memory counts as missing, so `SET lock x NX` works the moment the old lock runs out.
- `KEEPTTL` on a key with a deadline writes a `SETEX` with the same deadline, so it's crash-safe too. On a key without one it's just a `SET`.
- Compaction now writes each key that has a deadline as one `SETEX` instead of a `SET` plus an `EXPIRE`. That saves 22 bytes plus the key's length per key, and it made the size bookkeeping simpler: a deadline now always adds exactly 8 bytes to `liveBytes`. Logs with the old pairs still replay fine.
- Since the deadline now lives inside the value, a key can only get one if its value is at least 8 bytes under the 64 MB record limit. Otherwise `EXPIRE` and `SET ... EX` refuse it with "too large", rather than compaction failing on it later. (The server caps a command at 1 MB anyway, so only direct Go callers could get near that.)
- Options are matched case-insensitively and each group at most once. A third word that isn't an option (`SET k hello world` without quotes) is now an "unknown SET option" error rather than a usage line.

Tests: the option rules (NX/XX on missing, live and just-expired keys, KEEPTTL with and without a deadline, rounding, every bad combination); `TestSetWithCrashNeverLosesDeadline`, which cuts the log after every possible byte of a `SET ... NX EX` and checks the key is either missing or has its deadline, never there forever; a compaction test that checks the new log is exactly the size `liveBytes` predicted and comes back as one record per key; `SetWith` added to the random model test with random option mixes; and engine tests for the parsing and error messages. I broke it five ways on purpose (writing `SET` then `EXPIRE`, `KEEPTTL` dropping the deadline, not subtracting the 8 bytes when a deadline is cleared, compaction writing plain `SET`s, `NX` not treating an expired key as missing) and each one fails at least one test.

Not done yet: `SET ... GET` (return the old value) and `EXAT`/`PXAT` (an absolute deadline). The playground is still on the old WebAssembly build.

## 2026-11-21: Absolute deadlines and SET ... GET

Yesterday's two leftovers. `SET key value EXAT <unix-seconds>` (or `PXAT` in milliseconds) stores a value that expires at a set moment, `EXPIREAT` / `PEXPIREAT` do the same for a key that's already there, and `EXPIRETIME` / `PEXPIRETIME` read the deadline back as a Unix time (`-1` for none, `-2` for no key, like `TTL`). `SET ... GET` replies with the value it replaced instead of `OK`. The store side is `ExpireAt`, `Deadline` and `SetWithOld` in `internal/store/ttl.go`.

Why: a deadline often comes from somewhere else, like a token's `exp` field or the end of a sale, and turning it into "seconds from now" on the client is a race: the number is already stale by the time the command arrives. `SET ... GET` is the other half of a safe swap. Doing `GET` then `SET` from a client lets another client's write land in between, and you get back a value that was never the one you replaced.

How it works:

- No new log records. The log already stores deadlines as absolute Unix milliseconds, so `EXAT` writes the same `SETEX` as `EX` and `EXPIREAT` the same `EXPIRE` as `EXPIRE`. Only how the deadline is worked out changed.
- A moment that has already come deletes the key, the way Redis does. `EXPIREAT` writes a `DEL` and replies 1. `SET ... EXAT` with a past time replies `OK` but leaves the key gone, and on a key that didn't exist it writes nothing at all, since there's nothing to delete.
- Times between two milliseconds round up, same as durations, so a key never goes before the moment it was given. The "already past" check is done on the full time, before rounding. Otherwise `EXPIREAT key <now>` with the clock at 5.3 ms would round up to 6 ms and keep the key alive for a moment.
- The 100-year limit still holds: a deadline more than `MaxTTL` from now is refused, checked against the store's own clock under the lock. `EXAT` seconds are checked for overflow before they're multiplied into milliseconds.
- `SetWithOld` reads the old value and writes the new one under the same write lock, so nothing can slip between them. An expired key counts as having no old value. With `NX` on a live key, the write is refused but the old value still comes back (Redis 7 does this too), so `SET k v NX GET` tells you both that you lost and what you lost to. `SetWith` now just calls it and drops the extras.
- `EXPIRETIME` rounds down to the second, like Redis, while `PEXPIRETIME` is exact. `EXPIREAT` and `PEXPIREAT` are refused on `sql:` and `vec:` keys like every other write.

Tests: `ExpireAt` and `Deadline` on live, missing, expired and plain keys, sub-millisecond rounding, past moments (including with the clock between two milliseconds) and the 100-year limit; `SetWithOld` with `NX`/`XX`, expired keys and past deadlines; a restart test where some absolute deadlines pass while the database is closed; and the random model test from two days ago now also sends `EXAT` (some already past) and `ExpireAt`, checks every old value `SetWithOld` hands back, and checks `Deadline` for every key after every step. Engine tests cover parsing, replies and every error message. I broke it seven ways on purpose (no rounding up, `NX` dropping the old value, an expired key's value counted as old, no 100-year check, `EXPIRETIME` losing the milliseconds, and `EXPIREAT`/`SET EXAT` with "now" not deleting) and each one fails at least one test.

Not done yet: `GETDEL` and `GETEX`, and `EXPIRE`'s `NX`/`XX`/`GT`/`LT` options. The playground is still on the old WebAssembly build.

## 2026-11-22: GETDEL and GETEX

Yesterday's first leftover. `GETDEL key` reads a value and deletes the key in one step. `GETEX key` reads a value and changes its expiry in the same step: `EX`/`PX` for a time from now, `EXAT`/`PXAT` for a set moment, or `PERSIST` to take the expiry off. With no option `GETEX` is just a `GET`. Both reply `(nil)` for a missing key, like Redis. The store side is `GetDel` in `internal/store/store.go` and `GetEx` in `internal/store/ttl.go`.

Why: both are things clients get wrong when they have to do them in two commands. A one-time login code read with `GET` then `DEL` can be used twice if two requests arrive together, since both `GET`s land before either `DEL`. A session that should stay alive while it's being used needs its timeout pushed back on every read, and with `GET` then `EXPIRE` the key can run out in between, so the `EXPIRE` finds nothing and the user gets logged out mid-click.

How it works:

- Both take the write lock, check the key is live (an expired key counts as missing and gets dropped from memory), read the value and then write, so nothing can slip in between. That's what makes `GETDEL` safe for "only one client gets this".
- No new log records. `GETDEL` writes a `DEL` and `GETEX` an `EXPIRE`, the same records `DEL`, `EXPIRE`, `EXPIREAT` and `PERSIST` already write, so replay and compaction didn't change.
- They write nothing when there's nothing to change: a missing key, `GETEX` with no option (which only takes the read lock), and `GETEX ... PERSIST` on a key that has no expiry.
- `GETEX` follows the same rules as `EXPIRE` and `SET ... EX`: under a millisecond rounds up, more than 100 years is refused, and a moment that has already come deletes the key, but the value is still returned, which is what Redis does too.
- The number parsing for `EX`/`PX`/`EXAT`/`PXAT` moved out of `SET`'s option parser into one shared `parseExpiry`, so `SET` and `GETEX` give exactly the same error messages.
- `GETDEL` and `GETEX` with an option are refused on `sql:` and `vec:` keys like every other write. A bare `GETEX` is only a read, so it's allowed, same as `GET`.

Tests: `GetDel` on live, missing and expired keys, the log not growing for missing ones, and the delete holding across a restart; a race test where 8 goroutines `GetDel` the same key 200 times and exactly one wins each round; `GetEx` with every option, sub-millisecond rounding, a sliding timeout that keeps a session alive for 40 seconds on a 10-second expiry, past moments (with the clock between two milliseconds), bad option mixes, the 100-year limit, and a restart where the deadline it set comes back with the right time left. The random model test now also sends `GetDel` and `GetEx` with random options and checks every value they return. Engine tests cover the replies and every error message. I broke it seven ways on purpose (`GETDEL` dropping the key without logging a `DEL`, `PERSIST` logging when there's no expiry, a bare `GETEX` writing, rounding down, a past moment not deleting, `PERSIST` doing nothing, and no check that the key is live) and each one fails at least one test.

Not done yet: `EXPIRE`'s `NX`/`XX`/`GT`/`LT` options. The playground is still on the old WebAssembly build.

## 2026-11-23: EXPIRE options (NX, XX, GT, LT)

Yesterday's leftover. `EXPIRE`, `PEXPIRE`, `EXPIREAT` and `PEXPIREAT` now take `NX` (only if the key has no expiry), `XX` (only if it has one), `GT` (only if the new deadline is later) and `LT` (only if it's sooner). They reply `0` when a condition says no, like Redis 7. The store side is `ExpireWith` and `ExpireAtWith` in `internal/store/ttl.go`; plain `Expire` and `ExpireAt` now just call them with no conditions.

Why: the common case is a session or cache entry that several requests touch at once. Each one wants to push the deadline back, and with a plain `EXPIRE` a slow request carrying a shorter timeout can land last and pull it in again. `EXPIRE session 1800 GT` only ever moves it later, so the order requests arrive in stops mattering. `LT` is the opposite, a cap that can only tighten, and `NX` hands out a default expiry without overwriting one somebody already set. Checking the current deadline from the client and then calling `EXPIRE` is the same read-then-write race that `GETEX` fixed for reads.

How it works:

- No new log records. When the conditions pass it writes the same `EXPIRE` (or `DEL`) as before, and when they don't it writes nothing, so replay and compaction didn't change.
- The check and the write happen under the write lock, after the key has been checked as live. An expired key still counts as missing, so even `LT` replies `0` for it.
- A key with no expiry counts as living forever. So `GT` never applies to it (nothing is later than never) and `LT` always does. This is what Redis does, and it's what makes `LT` a real cap.
- The conditions are checked against the new deadline *before* deciding whether it has already passed, again like Redis. A zero, negative or past deadline counts as sooner than anything, so `EXPIRE k 0 LT` deletes the key and `EXPIRE k 0 GT` leaves it alone.
- Comparisons are in whole milliseconds after rounding up, and equal is neither later nor sooner. So `EXPIRE k 10 GT` on a key with exactly 10 seconds left is a `0`, even if the new one was a few microseconds longer before rounding.
- `XX` goes with `GT` or `LT`. `NX` with anything else, and `GT` with `LT`, are refused with an error, the same rules as Redis. Options can be in any case and order, each at most once.

Tests: a table that runs every option and mix against a key with no expiry and a key with 10 seconds left, with a new deadline that's sooner, equal, later, equal only after rounding, or already past, through both `ExpireWith` and `ExpireAtWith`; refused conditions not growing the log; bad mixes; the 100-year limit; expired keys; and a `GT` deadline coming back after a restart. The random model test from four days ago now sends random valid option mixes half the time and works out the expected answer on its own. Engine tests use a clock that stands still, so "the same deadline" is really the same millisecond between commands. I broke it seven ways on purpose (`<` instead of `<=` for `GT` and for `LT`, `GT` applying to keys with no expiry, `LT` refusing them, `XX` ignored, a past `EXPIREAT` treated as future, and `GT` with `LT` accepted) and each one fails at least one test.

Also updated the numbers at the top of the README, which had fallen behind since the 19th: 172 tests now, 91.6% coverage.

Not done yet: the playground is still on the old WebAssembly build, so none of the expiry commands from this week are on the site until I rebuild it.

## 2026-11-24: Atomic counters (INCR, DECR, INCRBY, DECRBY)

The expiry work is done, so today's a new small feature: counters. `INCR key` adds 1 to the number stored at a key and replies with the new value, `DECR` takes 1 away, and `INCRBY` / `DECRBY` add or take away any whole number. A missing key starts at 0. The store side is `IncrBy` in the new `internal/store/counter.go`; the four commands all call it (`DECRBY` just flips the sign).

Why: counting is one of the most common things people use a key-value store for (page views, likes, stock left, retries, rate limits), and it's easy to get wrong from a client. `GET views`, add one, `SET views` loses updates as soon as two clients do it at once: both read 41, both write 42. `INCR` does the read, the add and the write under the write lock, so they can't be split.

How it works:

- The value is still a plain string. Only the exact form `strconv.FormatInt` would write counts as a number, so `42` and `-7` do, but `+42`, `042`, `-0`, ` 42` and `4.2` are refused with `value is not an integer or out of range`. That keeps what's read and what's written back in the same form, and it's what Redis accepts too.
- A result that wouldn't fit in 64 bits is refused with `increment or decrement would overflow`, checked before adding so the sum itself can never wrap round. `DECRBY key -9223372036854775808` is refused in the engine, since flipping its sign doesn't fit either. Anything refused writes nothing.
- The key's expiry is kept, like Redis. That's what makes a rate limiter work: `INCR` a per-user key and give it a window with `EXPIRE ... NX` the first time, and the count resets on its own when the window ends. With an expiry, the new value goes to the log as one `SETEX` with the same deadline (the same trick as `SET ... KEEPTTL`), so a crash can't keep the count but lose the window. Without one it's a plain `SET`. No new log record types.
- An expired key counts as missing, so a counter whose window has passed starts again from 0 with no expiry.
- `INCR` and friends are refused on `sql:` and `vec:` keys like every other write.

Tests: `IncrBy` on missing keys, negative steps, every accepted and refused value form, overflow both ways right at the edge, the deadline being kept and coming back after a restart and after compaction then a restart, a counter starting over once its window passes, and refused increments not growing the log. A race test has 8 goroutines adding 500 times each and checks nothing was lost, before and after a restart. The random model test now also sends `IncrBy` with small steps and the odd overflowing one, checking every result and that the deadline stays. Engine tests cover all four commands, their replies and every error message. I broke it four ways on purpose (dropping the deadline, no overflow check, accepting `+5`/`05`-style values, and reading expired values as live) and each one fails at least one test.

Updated the README numbers too: 176 tests, 91.7% coverage.

Not done yet: `INCRBYFLOAT` for decimal counters. The playground is still on the old WebAssembly build.

## 2026-11-25: Decimal counters (INCRBYFLOAT)

Yesterday's leftover. `INCRBYFLOAT key 2.5` adds a decimal number to the value at a key and replies with the new value, so `INCRBYFLOAT balance -2.25` on `"10.5"` gives `"8.25"`. A missing key starts at 0. The store side is `IncrByFloat` in `internal/store/counter.go`, next to `IncrBy`.

Why: `INCR` covers counting, but plenty of running totals aren't whole numbers: a balance, seconds of watch time, a score with a weight. Without this, the only way to add 0.5 to a key is `GET`, add, `SET`, which has the same lost-update race `INCR` fixed yesterday.

How it works:

- Values are read and added as 64-bit floats, under the write lock like `IncrBy`. That means the normal float rounding: `0.1 + 0.2` is `0.30000000000000004`, and past 2^53 whole numbers lose their last digits. Redis uses 80-bit floats and rounds to 17 digits, so the last digit can differ from Redis. `INCR` is still exact all the way to 2^63, so it's the one to use for whole counts.
- Any plain decimal counts as a number: an optional sign, digits with an optional point, and an optional exponent (`5`, `+1.5`, `.5`, `5.`, `2.5e-3`). Spaces, `inf`, `nan`, hex floats, `_` and anything too big for a float64 are refused with `value is not a valid float`. I wrote a small hand check (`isDecimal`) instead of trusting `strconv.ParseFloat` alone, because that one happily accepts `Inf`, `NaN`, `0x1p-2` and `1_000`-style input.
- The result is written in the shortest plain decimal that reads back as the same float64 (`strconv.FormatFloat(f, 'f', -1, 64)`): no exponent, no `-0`, and no `.0` on whole numbers, so `INCR` keeps working on a key `INCRBYFLOAT` touched and the other way round.
- A result that would be infinite (or NaN) is refused with `increment would produce NaN or Infinity` and nothing is written.
- The key's expiry is kept, the same way as `IncrBy`. I pulled the "write the value, and the deadline too if there is one" part out into `setKeepDeadlineLocked`, so both counters share it. No new log record types.

Tests: every step and value form, accepted and refused, with refused values left unchanged; the 2^53 precision limit as an explicit case; non-finite results; the deadline kept across a restart and a compaction; refused increments not growing the log; a race test with 8 goroutines adding 0.5 or 0.25 (exact in binary, so the total has one right answer) before and after a restart; and a new fuzz test, `FuzzParseDecimal`, which checks that anything accepted is finite and that what gets written reads back as the same number. It ran about a million inputs clean. Engine tests cover the command, its replies and every error. I broke it eight ways on purpose (no `-0` fix, dropped deadline, out-of-range values accepted, infinite results accepted, exponent output, trailing junk accepted, empty exponent accepted, and the validator check on its own) and each one fails at least one test.

README numbers: 179 tests and 3 fuzz tests, 91.8% coverage.

Not done yet: the playground is still on the old WebAssembly build, so this week's commands aren't on the site.

## 2026-11-26: String commands (APPEND, STRLEN, GETRANGE)

The counters are done, so today's another small one: three commands that work on a value as a run of bytes. `APPEND key text` adds to the end and replies with the new length, `STRLEN key` gives the length, and `GETRANGE key start end` returns a slice. The store side is `Append`, `StrLen` and `GetRange` in the new `internal/store/strings.go`.

Why: appending is the other common read-then-write people do from a client (a log line, an audit trail, a small buffer). `GET`, concatenate, `SET` loses one of two appends that land at the same time, exactly like the counter race. `APPEND` does it under the write lock. `STRLEN` and `GETRANGE` are the cheap reads that go with it, so you can check the size of a value or read the last few bytes without pulling the whole thing over the network.

How it works:

- Everything counts bytes, not characters, like Redis. `café` is 5 bytes, and `GETRANGE` can cut the `é` in half. That keeps offsets exact and means no decoding.
- `GETRANGE` uses Redis offsets: both ends included, negative ones count back from the end (`0 -1` is all of it, `-3 -1` the last three bytes). Offsets past either end get pulled back to it, and anything that ends up empty returns `""`, as does a missing key. The slicing lives in a small `byteRange` function on its own so it can be fuzzed. Writing it out, I noticed that after clamping, "start past the end" and "end before the start" both come down to `start > end`, so that's the only check.
- `APPEND` on a missing key starts from `""`, so it creates the key (even when the text is empty, like Redis). Appending `""` to a key that exists writes nothing to the log.
- The key's expiry is kept, using the `setKeepDeadlineLocked` helper from yesterday, so the value and the deadline go to the log as one `SETEX`. No new log record types.
- A result over the 64 MB value limit is refused and nothing changes. With an expiry the limit is 8 bytes lower, because compaction writes the key as one `SETEX` with the deadline in front of the value.
- The log stores whole values, so each `APPEND` writes the full new value, not just the new part. Appending lots of small pieces to one key costs about n²/2 bytes of log until compaction clears the old copies. Fine for log lines and buffers, which is what this is for.

Tests: `Append` step by step (including a multi-byte character and `""`), `APPEND` on a number still leaving `INCR` working, the deadline being kept and coming back after a restart and after compaction then a restart, an expired key starting over with no deadline, the size limit with and without a deadline, and a race test with 8 goroutines appending 200 times each that checks every piece is there before and after a restart. `GetRange` has the examples from the Redis docs plus every edge I could think of, including `MinInt64`/`MaxInt64` offsets. A new fuzz test, `FuzzByteRange`, checks the fast version against a slow one that walks every byte; it ran about 1.5 million inputs clean. Engine tests cover all three commands, their replies and errors. I broke it seven ways on purpose (dropping the deadline, no size check, no room for the deadline, logging empty appends, two off-by-ones in the clamping, and slicing one byte short) and each one fails at least one test.

README numbers: 186 tests and 4 fuzz tests, coverage still 91.8%.

Not done yet: `SETRANGE`, which overwrites part of a value at an offset and pads with zero bytes. The playground is still on the old WebAssembly build.

## 2026-11-27: SETRANGE

Yesterday's leftover, and the last of the string commands. `SETRANGE key offset text` writes `text` over the value starting `offset` bytes in and replies with the new length, so `SETRANGE greeting 6 Redis` on `"Hello World"` gives `"Hello Redis"`. If the value is shorter than the offset, or the key is missing, the gap is filled with zero bytes first. The store side is `SetRange` in `internal/store/strings.go`, next to `Append`.

Why: `APPEND` covers adding to the end, but sometimes you want to patch a bit in the middle: fix a status byte in a fixed-width record, flip one slot in a small array of flags, or rewrite a header without touching the rest. Doing that from a client means `GET`, edit, `SET`, which has the same lost-update race as before. Two clients patching different parts of one value at once would each write back their own copy and one patch would vanish. `SETRANGE` does it under the write lock.

How it works:

- The byte work is in a small `overwriteAt` function: make a buffer as long as the bigger of the old value and `offset + len(text)`, copy the old value in, then copy `text` over it at the offset. Go zeroes new memory, so the padding comes for free.
- The size limit is checked before anything is allocated. `SETRANGE k 9223372036854775807 x` has to be refused straight away, not try to build a huge value or overflow `offset + len(text)`. Checking `offset` against the limit first means the addition after it can't overflow. With an expiry, the limit is 8 bytes lower, the same as `APPEND`; I pulled that rule out into `valueLimitLocked` so the two share it.
- A negative offset is refused with `offset is out of range`, like Redis.
- An empty `text` changes nothing and writes nothing, and unlike `APPEND` it doesn't create a missing key. That's what Redis does too. A write that leaves the bytes exactly as they were doesn't go to the log either.
- The key's expiry is kept through `setKeepDeadlineLocked`, so value and deadline go to the log as one `SETEX`. No new log record types.

Tests: the Redis doc examples, writing in the middle, at the end, past the end with padding, multi-byte text, a patched number still working with `INCR`, and a negative offset. The deadline is kept across a restart and a compaction, and an expired key starts over with no deadline. No-op writes don't grow the log. The size limit is checked at the exact edge, with `MaxInt64` offsets, and with and without a deadline. A race test has 8 goroutines each filling their own part of one shared value a byte at a time and checks nothing is lost, before and after a restart. A new fuzz test, `FuzzOverwriteAt`, checks the fast version against a slow one that works out every byte on its own; it ran about 2.7 million inputs clean. Engine tests cover the command, its replies and every error. I broke it seven ways on purpose (dropping the deadline, no room for the deadline, logging no-op writes, letting an empty text create the key, an off-by-one in the copy, reading an expired value as live, and loosening the limit check). Six failed a test straight away. The loose limit one didn't, because the final length check caught it anyway, so I simplified the code to one plain offset check plus that length check.

README numbers: 190 tests and 5 fuzz tests, 91.9% coverage.

Not done yet: the playground is still on the old WebAssembly build, so this week's commands aren't on the site.

## 2026-11-28: MSET, MSETNX, MGET, and batch log records

The string commands are done, so today I moved on to the multi-key ones. `MSET k1 v1 k2 v2 ...` sets a group of keys, `MSETNX` does the same only if none of them exist yet, and `MGET k1 k2 ...` reads a group back. The store side is `MSet`, `MSetNX` and `MGet` in the new `internal/store/multi.go`, and the log side is a new record type in `internal/aol/batch.go`.

Why: saving round trips is nice, but the real reason is that some values only make sense together. If you move 60 from one balance to another with two `SET`s, another client can read in between and see the money in neither place (or both), and a crash between the two writes leaves it like that for good. So `MSET` has to be all or nothing, both in memory and on disk.

How it works:

- In memory it's easy: everything happens under one write lock, and `MGET` reads all its keys under one read lock at the same moment. A reader can never land half way through an `MSET`.
- On disk it needed something new. Each record in the log already has two checksums and a torn record at the end is dropped on startup, so one record is already all or nothing. I added op 6, `BATCH`: its key is empty and its value packs several ordinary records one after another (op, key length, value length, key, value). `MSET` writes all its `SET`s as one batch, so a crash anywhere in the middle of writing it drops the whole thing on replay.
- `SplitBatch` is strict: a length running past the end, bytes left over, an unknown op or a batch inside a batch all count as malformed. A batch's total size has the same 64 MB limit as any value, checked one entry at a time so the running total can't overflow, so `MSET a <40 MB> b <40 MB>` is refused even though each value fits on its own.
- Replay just unpacks the batch and applies each record with the existing code, so the in-memory map and the byte counts for auto-compaction didn't need any new logic. Compaction still writes one record per key, so a compacted log never has a batch in it.
- An `MSET` of one pair writes a plain `SET` instead of a batch of one, which keeps the log smaller.
- Like `SET`, `MSET` removes any expiry, and a key listed twice keeps its last value. `MSETNX` treats an expired key as missing. A `sql:` or `vec:` key anywhere in the list refuses the whole command before anything is written.
- One catch I wrote down in the README: a build from before today doesn't know op 6, so it would call a log with a batch in it corrupt. Running `COMPACT` first removes all batches if I ever need to go back.

Tests: the aol side has a round trip for every op, the size limit at the exact edge, a list of malformed batches, a test that cuts a batch off at several points and checks replay drops all of it, and a new fuzz test, `FuzzSplitBatch`, which checks it never panics and that anything it accepts packs back into exactly the same bytes; it ran about 4.5 million inputs clean. On the store side, a crash test cuts the log at every single byte inside an `MSET` that moves a balance and checks that the old balances always come back. A race test has 4 writers moving a total of 100 between two keys while 4 readers check with `MGET` that it always adds up, with auto-compaction running and a restart at the end. Another has 16 callers racing `MSETNX` for overlapping pairs of seats and checks that every winner owns both its seats. I also broke it on purpose four ways (writing separate `SET`s instead of a batch, skipping the existence check in `MSETNX`, letting `MGET` read expired keys, and only applying part of a batch on replay), and each one fails a test; reading outside the lock is caught by the race detector.

README numbers: 203 tests and 6 fuzz tests, 92.0% coverage.

Next: the batch record is the building block for `RENAME` (a delete and a set that must happen together) and later for real transactions. The playground is still on the old WebAssembly build.
