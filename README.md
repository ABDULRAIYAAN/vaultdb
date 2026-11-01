# VaultDB

A database built from scratch in Go, one layer at a time: an in-memory key-value store first, then disk persistence, indexing, a query language, a network server and vector search.

The goal is to understand how real databases (Redis, Postgres, Bitcask, LevelDB, pgvector) work on the inside by building the same ideas by hand.

## Status

**v0.2.0: Milestone 2 complete.** Data is saved to an append-only log on disk and survives restarts and crashes.

## Quick start

You need [Go 1.22+](https://go.dev/dl/). On Windows you can also just double-click `run.bat`.

```bash
go run ./cmd/vaultdb
```

```
VaultDB 0.2.0. Type HELP for commands, EXIT to quit.
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
VaultDB 0.2.0. Type HELP for commands, EXIT to quit.
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

Run the tests (the `-race` flag checks for concurrency bugs; on Windows it needs a C compiler, so drop it if it errors):

```bash
go test -race ./...
```

## Commands

| Command | What it does |
|---|---|
| `SET <key> <value>` | Store a value. Use `"quotes"` for values with spaces |
| `GET <key>` | Read a value, or `(nil)` if missing |
| `DEL <key>` | Delete a key. Returns 1 if it existed, 0 if not |
| `EXISTS <key>` | 1 if the key exists, 0 if not |
| `KEYS` | List every key, sorted |
| `COUNT` | Number of keys |
| `CLEAR` | Delete everything |
| `INFO` | Storage mode, key count and log file size |
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
 splits text,                      checks arguments,            map in memory,        every change, in order,
 handles "quotes"                  formats the reply            RWMutex               replayed on startup
```

```
vaultdb/
  cmd/vaultdb/        entry point: the interactive shell and command-line flags
  internal/parser/    turns a line of text into a command
  internal/engine/    runs commands, returns replies (reused by the TCP server later)
  internal/store/     the data: in-memory map plus the write-ahead rule
  internal/aol/       the append-only log file: record format, append, replay, crash recovery
```

### How persistence works

Data is never edited in place on disk. Every change is **appended** to the end of `data/vaultdb.log` as a record. On startup VaultDB reads the log from the beginning and **replays** every record, which rebuilds the in-memory map exactly as it was. Reads are served from memory and never touch the disk.

Each record is binary, with a fixed 9-byte header:

```
+--------+-------------+-------------+-----------+-------------+
| op (1) | keyLen (4)  | valLen (4)  | key bytes | value bytes |
+--------+-------------+-------------+-----------+-------------+
 op: 1 = SET, 2 = DEL, 3 = CLEAR      lengths are big-endian uint32
```

Length prefixes (instead of, say, one line of text per record) mean keys and values can contain any bytes, including spaces, newlines and quotes, without escaping.

### Design notes

- **Write-ahead rule.** A write goes to the log first and to memory second. If the disk write fails, memory is left unchanged, so the database never reports data that isn't saved.
- **fsync on every write.** `Write` only hands data to the operating system, which may keep it in RAM for a while. `fsync` forces it onto the physical disk, so once VaultDB replies `OK`, a power cut can't lose that write. This costs speed; `-nosync` trades safety for speed, which is how many databases offer it.
- **Torn-write recovery.** If the machine dies halfway through writing a record, the file ends in a partial record. Replay detects it, cuts the file back to the last complete record and starts normally.
- **Corruption is refused, not guessed.** An impossible operation or length in the middle of the file stops startup with an error instead of loading wrong data. Milestone 3 adds checksums to catch subtler damage.
- **The store is thread-safe.** A `sync.RWMutex` lets many readers in at once but gives writers exclusive access. The write lock covers both the log append and the map update, so the log and memory always see changes in the same order.
- **The engine returns strings instead of printing.** The same engine will serve both the shell and the TCP server, so it should not know where its output goes.

### Known limitation (fixed in milestone 3)

The log only grows. Setting the same key 1,000 times stores 1,000 records even though only the last one matters. Milestone 3 adds **compaction**: rewriting the log with just the current data.

## Roadmap

- [x] 1. In-memory key-value store and interactive shell
- [x] 2. Persistence: append-only log, so data survives a restart
- [ ] 3. Crash recovery (checksums) and log compaction
- [ ] 4. Indexing: hash index, then a B-tree for range queries
- [ ] 5. A small SQL-like query language (`CREATE TABLE`, `INSERT`, `SELECT ... WHERE`)
- [ ] 6. TCP server so multiple clients can connect
- [ ] 7. Vector search: store embeddings, nearest-neighbour queries (brute force, then HNSW)
- [ ] 8. Benchmarks

## Author

Abdul Raiyaan, [GitHub](https://github.com/ABDULRAIYAAN) · [LinkedIn](https://linkedin.com/in/abdulraiyaan)
