# VaultDB

A database built from scratch in Go, one layer at a time: an in-memory key-value store first, then disk persistence, indexing, a query language, a network server and vector search.

The goal is to understand how real databases (Redis, Postgres, Bitcask, LevelDB, pgvector) work on the inside by building the same ideas by hand.

## Status

**v0.6.0: Milestone 6 complete.** VaultDB runs as a network server that many clients can use at once, and speaks SQL: tables, typed columns, primary keys, `WHERE`, `ORDER BY`, `LIMIT`, `UPDATE` and `DELETE`, all stored as ordinary keys in a B-tree and saved to a checksummed, crash-safe, self-compacting log.

## Quick start

You need [Go 1.22+](https://go.dev/dl/). On Windows you can also just double-click `run.bat`.

```bash
go run ./cmd/vaultdb
```

```
VaultDB 0.6.0. Type HELP for commands, EXIT to quit.
Loaded 0 keys from data/vaultdb.log (0 log records).
vaultdb> SET name raiyaan
OK
vaultdb> SET bio "CS student"
OK
vaultdb> EXIT
Bye.
```

Start it again and the data is still there:

```
VaultDB 0.6.0. Type HELP for commands, EXIT to quit.
Loaded 2 keys from data/vaultdb.log (2 log records).
vaultdb> GET name
"raiyaan"
```

Options:

| Flag | Meaning |
|---|---|
| `-data <path>` | Log file to use (default `data/vaultdb.log`) |
| `-memory` | Keep everything in memory and save nothing |
| `-nosync` | Skip fsync after each write: faster, but a power cut can lose the last few writes |
| `-repair` | Start even if the log is damaged, keeping everything before the damage |
| `-autocompact=false` | Never compact automatically (use the `COMPACT` command instead) |

Run the tests (the `-race` flag checks for concurrency bugs; on Windows it needs a C compiler, so drop it if it errors):

```bash
go test -race ./...
```

## Running as a server

```bash
go run ./cmd/vaultdb -serve :7379          # terminal 1: the server
go run ./cmd/vaultdb-cli                   # terminal 2: interactive client
go run ./cmd/vaultdb-cli -c "GET name"     # one command, for scripts
```

On Windows, double-click `server.bat`, then `cli.bat` (open several to see many clients at once).

```
localhost:7379> SET name raiyaan
OK
localhost:7379> GET name
"raiyaan"
```

### The protocol

Plain TCP, simple enough to use from any language:

```
client -> server   one command per line            SET name raiyaan\n
server -> client   reply length, newline, reply    2\nOK
```

Sending the length first means a reply can contain newlines (like a SQL result table) and the client still knows exactly where it ends, the same idea as Redis's bulk strings. `PING` answers `PONG`; `QUIT` closes the connection.

### How it handles many clients

- **One goroutine per client.** Goroutines are Go's lightweight threads, so hundreds of clients cost little. The store's read/write lock keeps their reads and writes safe.
- **SQL statements run one at a time.** An `INSERT` checks that a primary key is free and then writes the row. Without serialising, two clients could both see the key as free and both write it. A test runs 50 clients racing to insert the same 20 ids and checks each id is inserted exactly once.
- **Protection against bad clients:** commands over 1 MB are rejected without buffering them, silent clients are disconnected after 5 minutes, and connections are capped at 1,000.
- **Graceful shutdown:** Ctrl+C stops accepting new clients, lets running commands finish, disconnects everyone, then closes the log file cleanly.

## SQL

```
vaultdb> CREATE TABLE students (id INT PRIMARY KEY, name TEXT, gpa FLOAT)
OK, table students created
vaultdb> INSERT INTO students VALUES (1, 'Raiyaan', 3.9), (2, 'Ali', 3.2), (3, 'Sara', 3.6)
OK, 3 rows inserted
vaultdb> SELECT name, gpa FROM students WHERE gpa > 3.5 ORDER BY gpa DESC
+-----------+-----+
| name      | gpa |
+-----------+-----+
| 'Raiyaan' | 3.9 |
| 'Sara'    | 3.6 |
+-----------+-----+
(2 rows)
```

Supported: `CREATE TABLE`, `DROP TABLE`, `SHOW TABLES`, `INSERT` (several rows at once, optional column list), `SELECT` (columns, `*`, `COUNT(*)`, `WHERE`, `ORDER BY ... ASC|DESC`, `LIMIT`), `UPDATE ... SET ... WHERE`, `DELETE ... WHERE`. Types: `INT`, `FLOAT`, `TEXT`, `BOOL`, plus `NULL`. Conditions: `= != <> < <= > >=`, `AND`, `OR`, `NOT`, `IS [NOT] NULL` and brackets.

### How SQL runs

```
"SELECT name FROM students WHERE gpa > 3.5"
        |
   lexer      -> tokens: SELECT  name  FROM  students  WHERE  gpa  >  3.5
        |
   parser     -> tree:   Select{Columns:[name], Table:students, Where: gpa > 3.5}
        |
   executor   -> reads rows from the key-value store, filters, sorts, picks columns
```

**Tables are just keys.** There is no separate table engine, the same approach used by CockroachDB and TiDB:

```
sql:schema:students                   {"columns":[{"name":"id","type":"INT","pk":true}, ...]}
sql:row:students:8000000000000001     [1,"Raiyaan",3.9]
sql:row:students:8000000000000002     [2,"Ali",3.2]
```

Because the store is a B-tree, a table's rows sit next to each other, so a full table read is one prefix walk. `WHERE id = 2` on the primary key skips the walk and does a single key lookup. Integer keys are written as 16 hex digits with the sign bit flipped, so key order matches number order, negatives included.

Rules the executor enforces: types must match the column (an `INT` is accepted in a `FLOAT` column), primary keys must be unique and not `NULL`, a multi-row `INSERT` is checked completely before any row is written, and comparing with `NULL` is never true (use `IS NULL`), as in standard SQL.

## Commands

| Command | What it does |
|---|---|
| `SET <key> <value>` | Store a value. Use `"quotes"` for values with spaces |
| `GET <key>` | Read a value, or `(nil)` if missing |
| `DEL <key>` | Delete a key. Returns 1 if it existed, 0 if not |
| `EXISTS <key>` | 1 if the key exists, 0 if not |
| `KEYS` | List every key, sorted |
| `SCAN <from> <to> [limit]` | Keys from `<from>` up to (not including) `<to>`, sorted. `""` means no start or no end |
| `PREFIX <prefix> [limit]` | Keys starting with `<prefix>`, sorted |
| `COUNT` | Number of keys |
| `CLEAR` | Delete everything |
| `INFO` | Keys, log size, live data size, garbage percentage, compactions |
| `COMPACT` | Rewrite the log with only the current data |
| `HELP` | Show the command list |
| `EXIT` | Quit |

## Architecture

```
 input line
     |
     v
 +---------+  Command{Name, Args}  +---------+  Set/Get/Delete  +---------+  append   +----------------+
 | parser  | --------------------> | engine  | ---------------> |  store  | --------> | aol (log file) |
 +---------+                       +---------+                  +---------+           +----------------+
 splits text,                      checks arguments,            B-tree in memory,     every change, in order,
 handles "quotes"                  formats the reply            RWMutex               replayed on startup
```

```
vaultdb/
  cmd/vaultdb/        entry point: the interactive shell and command-line flags
  internal/parser/    turns a line of text into a command
  internal/engine/    runs commands, returns replies (reused by the TCP server later)
  internal/store/     the data: B-tree in memory, write-ahead rule, compaction
  internal/btree/     sorted in-memory index: insert with splits, delete with borrow/merge, range walks
  internal/sql/       SQL lexer, recursive descent parser, AST, executor, result tables
  internal/server/    TCP server: goroutine per client, length-prefixed replies, limits, graceful shutdown
  internal/client/    Go client library for the protocol
  cmd/vaultdb-cli/    command-line client (interactive or one-shot with -c)
  internal/aol/       the log file: record format, checksums, append, replay, crash recovery, atomic rewrite
```

### How persistence works

Data is never edited in place on disk. Every change is **appended** to the end of `data/vaultdb.log` as a record. On startup VaultDB reads the log from the beginning and **replays** every record, which rebuilds the in-memory map exactly as it was. Reads are served from memory and never touch the disk.

The file starts with a 9-byte header (`VAULTDB\0` plus a format version byte), followed by records. Each record is binary:

```
+------------+--------+------------+------------+-------------+-----+-------+
| hdrCRC (4) | op (1) | keyLen (4) | valLen (4) | bodyCRC (4) | key | value |
+------------+--------+------------+------------+-------------+-----+-------+
 op: 1 = SET, 2 = DEL, 3 = CLEAR          all integers big-endian
 hdrCRC  = CRC-32C of op, keyLen, valLen, bodyCRC
 bodyCRC = CRC-32C of key and value
```

Length prefixes (instead of, say, one line of text per record) mean keys and values can contain any bytes, including spaces, newlines and quotes, without escaping.

**Why two checksums?** With a single checksum over the whole record, a flipped bit in a length field would make the reader trust a wrong length before it could check anything. It could then read past the real end of the record, decide the file was cut off, and throw away every valid record after it. Verifying the small fixed-size header first means a length is only used once it is known to be correct.

### How the B-tree index works

Keys are kept in a **B-tree**, the structure behind the indexes in Postgres, MySQL and SQLite. Each node holds a sorted list of 31 to 63 keys, and a node with *n* keys has *n + 1* children holding the keys that fall between them:

```
                  [ g | p ]
                 /    |    \
        [a | c | e] [h | k] [q | t | x]
```

- **Lookup:** start at the root, binary-search inside the node, step into the right child. 100,000 keys need at most 4 levels (about 170 ns per lookup).
- **Insert:** a full node is split in two on the way down and its middle key moves up. The tree only grows at the root, so every leaf stays at the same depth.
- **Delete:** before stepping into a node with the minimum number of keys, it borrows a key from a sibling or merges with one, so no node is ever left too empty.
- **Range scans:** `SCAN` and `PREFIX` jump straight to the first matching key and walk forward in order. A hash map would have to check every key.

### How compaction works

Overwriting a key 1,000 times leaves 999 dead records in the log. VaultDB tracks how big the log *would* be with one `SET` per live key, and when the real file is at least 1 MB and more than twice that size, it compacts:

1. Write the current data to a temporary file, `vaultdb.log.compact`.
2. `fsync` the temporary file so it is fully on disk.
3. Rename it over `vaultdb.log`.

A rename within one folder is **atomic**: at every moment the path holds either the complete old log or the complete new one, never a mix. If the machine dies before step 3, the old log is untouched and the leftover temporary file is deleted on the next start.

### Design notes

- **Write-ahead rule.** A write goes to the log first and to memory second. If the disk write fails, memory is left unchanged, so the database never reports data that isn't saved.
- **fsync on every write.** `Write` only hands data to the operating system, which may keep it in RAM for a while. `fsync` forces it onto the physical disk, so once VaultDB replies `OK`, a power cut can't lose that write. This costs speed; `-nosync` trades safety for speed, which is how many databases offer it.
- **Torn-write recovery.** If the machine dies halfway through writing a record, the file ends in a partial record. Replay detects it, cuts the file back to the last complete record and starts normally.
- **Corruption is refused, not guessed.** A checksum failure in the middle of the file stops startup with an error and the exact byte offset, instead of loading wrong data. `-repair` keeps everything before the damage.
- **Format upgrades are automatic.** A log written by VaultDB 0.2 (no header, no checksums) is detected, read, and rewritten in the new format on first start.
- **Windows-safe file swapping.** Windows refuses to rename over a file that is still open, so compaction closes the log before the rename and reopens whichever file ends up at the path, even if the rename fails.
- **The store is thread-safe.** A `sync.RWMutex` lets many readers in at once but gives writers exclusive access. The write lock covers both the log append and the map update, so the log and memory always see changes in the same order.
- **The engine returns strings instead of printing.** The same engine will serve both the shell and the TCP server, so it should not know where its output goes.

## Roadmap

- [x] 1. In-memory key-value store and interactive shell
- [x] 2. Persistence: append-only log, so data survives a restart
- [x] 3. Crash recovery (checksums) and log compaction
- [x] 4. Indexing: B-tree for sorted keys, range and prefix queries
- [x] 5. A small SQL-like query language (`CREATE TABLE`, `INSERT`, `SELECT ... WHERE`, `UPDATE`, `DELETE`)
- [x] 6. TCP server so multiple clients can connect
- [ ] 7. Vector search: store embeddings, nearest-neighbour queries (brute force, then HNSW)
- [ ] 8. Benchmarks

## Author

Abdul Raiyaan, [GitHub](https://github.com/ABDULRAIYAAN) · [LinkedIn](https://linkedin.com/in/abdulraiyaan)
