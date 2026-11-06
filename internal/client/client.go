// Package client connects to a VaultDB server.
//
//	c, err := client.Dial("localhost:7379")
//	reply, err := c.Do("SET name raiyaan")   // "OK"
//	reply, err = c.Do("GET name")            // "\"raiyaan\""
package client

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxReply is the largest reply the client will accept (64 MB).
const MaxReply = 64 << 20

// Client is one connection to a server. It is safe to share between
// goroutines: commands are sent one at a time.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

// Dial connects to a server, giving up after 5 seconds.
func Dial(addr string) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, r: bufio.NewReader(conn)}, nil
}

// Do sends one command and returns the server's reply.
func (c *Client) Do(command string) (string, error) {
	if strings.ContainsAny(command, "\r\n") {
		return "", errors.New("a command must be a single line")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := io.WriteString(c.conn, command+"\n"); err != nil {
		return "", err
	}
	header, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || n < 0 || n > MaxReply {
		return "", fmt.Errorf("bad reply header %q", header)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return "", err
	}
	return string(body), nil
}

// Close ends the connection.
func (c *Client) Close() error { return c.conn.Close() }
