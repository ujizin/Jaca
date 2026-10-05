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
	closed  bool // set once readLoop ends; a Call after that fails at once
	Events  chan event
	Closed  chan struct{}

	// Events are queued here and pumped into Events, so readLoop never blocks on a busy UI
	// (a blocked readLoop would also stall the replies a pane's own Call waits for).
	qmu     sync.Mutex
	qcond   *sync.Cond
	queue   []event
	dropped map[string]int // events skipped since the queue was last full, by topic
}

// eventQueueCap bounds the client-side event queue. Past it, events are skipped and the pane is
// told with a synthetic events.dropped, the way jacad reports a slow connection.
const eventQueueCap = 4096

// callTimeout is how long Call waits for a reply.
const callTimeout = 30 * time.Second

// droppedNote is the events.dropped payload (DroppedEvents in DaemonEventBus.swift).
type droppedNote struct {
	Topic string `json:"topic"`
	Count int    `json:"count"`
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
	c := &client{conn: conn, pending: map[int]chan inbound{}, Events: make(chan event), Closed: make(chan struct{}),
		dropped: map[string]int{}}
	c.qcond = sync.NewCond(&c.qmu)
	go c.readLoop()
	go c.pump()
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
		c.closed = true
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		close(c.Closed)
		c.qmu.Lock()
		c.qcond.Broadcast()
		c.qmu.Unlock()
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
			c.inject(event{Topic: msg.Params.Topic, Data: msg.Params.Data})
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

// inject queues an event for the pane. When the queue is full the event is skipped and counted;
// an events.dropped note for its topic is queued as soon as there is room again, ahead of any
// later event.
func (c *client) inject(ev event) {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	if len(c.queue) >= eventQueueCap {
		c.dropped[ev.Topic]++
		return
	}
	c.flushDropped()
	c.queue = append(c.queue, ev)
	c.qcond.Signal()
}

// flushDropped queues the pending events.dropped notes. Called with qmu held.
func (c *client) flushDropped() {
	for topic, n := range c.dropped {
		c.queue = append(c.queue, event{Topic: "events.dropped", Data: mustJSON(droppedNote{Topic: topic, Count: n})})
		delete(c.dropped, topic)
	}
}

// pump hands queued events to Events in order until the connection closes.
func (c *client) pump() {
	for {
		c.qmu.Lock()
		for len(c.queue) == 0 {
			select {
			case <-c.Closed:
				c.qmu.Unlock()
				return
			default:
			}
			c.qcond.Wait()
		}
		ev := c.queue[0]
		c.queue[0] = event{}
		c.queue = c.queue[1:]
		c.flushDropped()
		c.qmu.Unlock()
		select {
		case c.Events <- ev:
		case <-c.Closed:
			return
		}
	}
}

// Call sends a request and decodes its result into out (which may be nil), waiting up to
// callTimeout for the reply.
func (c *client) Call(method string, params any, out any) error {
	return c.CallTimeout(method, params, out, callTimeout)
}

// CallTimeout is Call with its own wait, for methods that legitimately run long.
func (c *client) CallTimeout(method string, params any, out any, timeout time.Duration) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("jacad closed the connection")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan inbound, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	line, err := json.Marshal(req)
	if err != nil {
		forget()
		return err
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		forget()
		return err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var msg inbound
	select {
	case m, ok := <-ch:
		if !ok {
			return errors.New("jacad closed the connection")
		}
		msg = m
	case <-timer.C:
		forget()
		return fmt.Errorf("%s: no reply from jacad after %s", method, timeout)
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
