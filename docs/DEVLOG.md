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
