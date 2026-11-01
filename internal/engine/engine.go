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
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

// HelpText lists every supported command.
const HelpText = `Commands:
  SET <key> <value>   store a value (use "quotes" for spaces)
  GET <key>           read a value
  DEL <key>           delete a key
  EXISTS <key>        1 if the key exists, 0 if not
  KEYS                list every key
  COUNT               number of keys
  CLEAR               delete everything
  INFO                storage mode, key count, log size
  HELP                show this list
  EXIT                quit`

// Engine executes commands.
type Engine struct {
	store *store.Store
}

// New creates an Engine backed by s.
func New(s *store.Store) *Engine {
	return &Engine{store: s}
}

// Execute runs cmd and returns the text to show the user.
func (e *Engine) Execute(cmd parser.Command) string {
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
		return "OK"

	case "INFO":
		if len(cmd.Args) != 0 {
			return usage("INFO")
		}
		mode := "in-memory (nothing saved)"
		if e.store.Persistent() {
			mode = "persistent (append-only log)"
		}
		return fmt.Sprintf("mode: %s\nkeys: %d\nlog size: %s",
			mode, e.store.Len(), formatBytes(e.store.LogSize()))

	case "HELP":
		return HelpText

	default:
		return fmt.Sprintf("(error) unknown command %q, type HELP for the list", cmd.Name)
	}
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
