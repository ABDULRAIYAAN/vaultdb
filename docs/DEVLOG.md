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
