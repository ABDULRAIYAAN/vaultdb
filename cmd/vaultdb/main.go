// Command vaultdb starts an interactive VaultDB shell.
//
// Usage:
//
//	vaultdb                         save data to data/vaultdb.log (default)
//	vaultdb -data path/to/file.log  save data somewhere else
//	vaultdb -memory                 keep everything in memory, save nothing
//	vaultdb -nosync                 faster writes, but a power cut may lose
//	                                the last few writes
//	vaultdb -repair                 start even if the log is damaged, keeping
//	                                everything before the damage
//	vaultdb -autocompact=false      never compact automatically
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
	"github.com/ABDULRAIYAAN/vaultdb/internal/engine"
	"github.com/ABDULRAIYAAN/vaultdb/internal/parser"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

const version = "0.4.0"

func main() {
	dataPath := flag.String("data", filepath.Join("data", "vaultdb.log"), "log file to store data in")
	memory := flag.Bool("memory", false, "in-memory only: nothing is saved to disk")
	noSync := flag.Bool("nosync", false, "skip fsync after each write (faster, less safe)")
	repair := flag.Bool("repair", false, "start even if the log is corrupt, keeping the data before the damage")
	autoCompact := flag.Bool("autocompact", true, "compact the log automatically when it is mostly garbage")
	flag.Parse()

	opts := store.DefaultOptions()
	opts.Sync = !*noSync
	opts.Repair = *repair
	opts.AutoCompact = *autoCompact

	db, banner, err := openStore(*dataPath, *memory, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not open database:", err)
		if errors.Is(err, aol.ErrCorrupt) {
			fmt.Fprintln(os.Stderr, "The log file is damaged. Back it up, then run with -repair to keep everything before the damage.")
		}
		os.Exit(1)
	}

	// Ctrl+C: close the log cleanly before exiting.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		<-interrupts
		db.Close()
		fmt.Println("\nBye.")
		os.Exit(0)
	}()

	eng := engine.New(db)
	fmt.Printf("VaultDB %s. Type HELP for commands, EXIT to quit.\n", version)
	fmt.Println(banner)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // allow values up to 1 MB

	for {
		fmt.Print("vaultdb> ")
		if !scanner.Scan() {
			fmt.Println()
			break // Ctrl+D / Ctrl+Z or end of piped input
		}

		cmd, err := parser.Parse(scanner.Text())
		if err != nil {
			fmt.Println("(error)", err)
			continue
		}
		if cmd.Name == "" {
			continue
		}
		if cmd.Name == "EXIT" || cmd.Name == "QUIT" {
			fmt.Println("Bye.")
			break
		}
		fmt.Println(eng.Execute(cmd))
	}

	if err := db.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "error closing database:", err)
		os.Exit(1)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "read error:", err)
		os.Exit(1)
	}
}

// openStore opens the right kind of store and returns a one-line description.
func openStore(path string, memory bool, opts store.Options) (*store.Store, string, error) {
	if memory {
		return store.New(), "Mode: in-memory. Nothing will be saved.", nil
	}
	db, info, err := store.Open(path, opts)
	if err != nil {
		return nil, "", err
	}
	banner := fmt.Sprintf("Loaded %d keys from %s (%d log records).", info.Keys, info.Path, info.Records)
	if info.TornBytes > 0 {
		banner += fmt.Sprintf("\nRecovered from a crash: dropped %d bytes of an unfinished write.", info.TornBytes)
	}
	if info.RepairedBytes > 0 {
		banner += fmt.Sprintf("\nRepair mode: cut off %d damaged bytes. Everything before the damage was kept.", info.RepairedBytes)
	}
	if info.Upgraded {
		banner += "\nUpgraded the log file to the new checksummed format."
	}
	return db, banner, nil
}
