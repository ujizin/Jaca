package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// protocolVersion must match DaemonProtocol.version in Sources/Core/Daemon/DaemonProtocol.swift.
const protocolVersion = 1

// rpcError is a JSON-RPC error object from jacad.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

type inbound struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Params *struct {
		Topic string          `json:"topic"`
		Data  json.RawMessage `json:"data"`
	} `json:"params"`
}

// event is one message on a subscribed topic.
type event struct {
	Topic string
	Data  json.RawMessage
}

// client is a newline-delimited JSON-RPC connection to jacad's Unix socket.
type client struct {
	conn    net.Conn
	mu      sync.Mutex
	nextID  int
	pending map[int]chan inbound
	Events  chan event
	Closed  chan struct{}
}

func daemonDir() string {
	if d := os.Getenv("JACA_DAEMON_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".jaca")
}

func socketPath() string { return filepath.Join(daemonDir(), "jacad.sock") }

// jacadPath finds the jacad binary: $JACAD_PATH, then the installed Jaca.app.
func jacadPath() (string, error) {
	candidates := []string{os.Getenv("JACAD_PATH"), "/Applications/Jaca.app/Contents/MacOS/jacad"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Applications/Jaca.app/Contents/MacOS/jacad"))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("jacad not found: install Jaca.app in /Applications or set JACAD_PATH")
}

// ensureDaemon starts jacad if its socket isn't answering. `jacad call ping` spawns it.
func ensureDaemon() error {
	if c, err := net.DialTimeout("unix", socketPath(), time.Second); err == nil {
		c.Close()
		return nil
	}
	bin, err := jacadPath()
	if err != nil {
		return err
	}
	out, err := exec.Command(bin, "call", "ping").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dial connects (starting jacad if needed) and performs the hello handshake.
func dial() (*client, error) {
	if err := ensureDaemon(); err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		return nil, err
	}
	c := &client{conn: conn, pending: map[int]chan inbound{}, Events: make(chan event, 256), Closed: make(chan struct{})}
	go c.readLoop()
	var hello struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := c.Call("hello", map[string]any{"protocolVersion": protocolVersion, "client": "jaca-herdr"}, &hello); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *client) readLoop() {
	defer func() {
		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		close(c.Closed)
	}()
	r := bufio.NewReaderSize(c.conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg inbound
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if msg.Method == "event" && msg.Params != nil {
			select {
			case c.Events <- event{Topic: msg.Params.Topic, Data: msg.Params.Data}:
			default: // the UI is behind; the next retained event carries the full state
			}
			continue
		}
		if msg.ID == nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[*msg.ID]
		delete(c.pending, *msg.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
	}
}

// Call sends a request and decodes its result into out (which may be nil).
func (c *client) Call(method string, params any, out any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan inbound, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return err
	}
	msg, ok := <-ch
	if !ok {
		return errors.New("jacad closed the connection")
	}
	if msg.Error != nil {
		return msg.Error
	}
	if out != nil {
		return json.Unmarshal(msg.Result, out)
	}
	return nil
}

func (c *client) Subscribe(topics ...string) error {
	return c.Call("events.subscribe", map[string]any{"topics": topics}, nil)
}

func (c *client) Close() { c.conn.Close() }
