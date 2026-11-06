// Command vaultdb-cli connects to a VaultDB server.
//
//	vaultdb-cli                          interactive, connects to localhost:7379
//	vaultdb-cli -addr 10.0.0.5:7379      connect somewhere else
//	vaultdb-cli -c "GET name"            run one command and exit (for scripts)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ABDULRAIYAAN/vaultdb/internal/client"
)

func main() {
	addr := flag.String("addr", "localhost:7379", "server address")
	command := flag.String("c", "", "run this one command, print the reply and exit")
	flag.Parse()

	c, err := client.Dial(*addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not connect to %s: %v\n", *addr, err)
		fmt.Fprintln(os.Stderr, "Is the server running? Start it with: vaultdb -serve :7379")
		os.Exit(1)
	}
	defer c.Close()

	if *command != "" {
		reply, err := c.Do(*command)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println(reply)
		if strings.HasPrefix(reply, "(error)") {
			os.Exit(1)
		}
		return
	}

	fmt.Printf("Connected to VaultDB at %s. Type HELP for commands, EXIT to quit.\n", *addr)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for {
		fmt.Printf("%s> ", *addr)
		if !scanner.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		reply, err := c.Do(line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connection lost:", err)
			os.Exit(1)
		}
		fmt.Println(reply)
		if u := strings.ToUpper(line); u == "EXIT" || u == "QUIT" {
			return
		}
	}
}
