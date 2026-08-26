// Package render drives a headless Chrome to produce the DOM a browser would
// build, for sites whose content only exists after JavaScript has run.
//
// It speaks the Chrome DevTools Protocol directly rather than depending on a
// browser-automation framework: the crawler needs three operations (navigate,
// watch network activity, read the DOM), and a direct client keeps the moving
// parts and the dependency surface small.
package render

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// message is one frame of the protocol. A frame carrying an id is a reply to a
// command; one carrying a method is an event.
type message struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *protocolError  `json:"error,omitempty"`
}

type protocolError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *protocolError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("devtools error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("devtools error %d: %s", e.Code, e.Message)
}

// event is a protocol notification delivered to a session subscriber.
type event struct {
	Method string
	Params json.RawMessage
}

// conn multiplexes commands and events over the single websocket Chrome
// exposes. Every tab is a "session" on that one socket, which is why the flat
// protocol mode is used: one connection, many pages.
type conn struct {
	ws *websocket.Conn

	writeMu sync.Mutex
	nextID  atomic.Int64

	mu       sync.Mutex
	pending  map[int64]chan message
	sessions map[string]chan event
	closed   bool
	closeErr error

	done chan struct{}
}

func newConn(ws *websocket.Conn) *conn {
	// A rendered DOM is routinely several megabytes; the cap is a guard against
	// a runaway page, not a working limit.
	ws.SetReadLimit(256 << 20)
	c := &conn{
		ws:       ws,
		pending:  map[int64]chan message{},
		sessions: map[string]chan event{},
		done:     make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *conn) readLoop() {
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.shutdown(err)
			return
		}
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}

		if m.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ok {
				ch <- m
			}
			continue
		}
		if m.Method == "" {
			continue
		}

		c.mu.Lock()
		ch, ok := c.sessions[m.SessionID]
		c.mu.Unlock()
		if !ok {
			continue
		}
		// Events are dropped rather than allowed to block the reader: a slow
		// consumer must never stall every other tab on the connection.
		select {
		case ch <- event{Method: m.Method, Params: m.Params}:
		default:
		}
	}
}

func (c *conn) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	pending := c.pending
	c.pending = map[int64]chan message{}
	sessions := c.sessions
	c.sessions = map[string]chan event{}
	c.mu.Unlock()

	close(c.done)
	for _, ch := range pending {
		close(ch)
	}
	for _, ch := range sessions {
		close(ch)
	}
}

// call sends a command and waits for its reply. An empty sessionID addresses
// the browser itself rather than a page.
func (c *conn) call(ctx context.Context, sessionID, method string, params any, result any) error {
	id := c.nextID.Add(1)
	reply := make(chan message, 1)

	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("devtools connection is closed")
		}
		return err
	}
	c.pending[id] = reply
	c.mu.Unlock()

	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	if sessionID != "" {
		payload["sessionId"] = sessionID
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	err = c.ws.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.done:
		if c.closeErr != nil {
			return c.closeErr
		}
		return fmt.Errorf("%s: devtools connection closed", method)
	case m, ok := <-reply:
		if !ok {
			return fmt.Errorf("%s: devtools connection closed", method)
		}
		if m.Error != nil {
			return fmt.Errorf("%s: %w", method, m.Error)
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	}
}

// subscribe registers an event channel for a session.
func (c *conn) subscribe(sessionID string, buffer int) (<-chan event, func()) {
	ch := make(chan event, buffer)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	c.sessions[sessionID] = ch
	c.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			c.mu.Lock()
			if existing, ok := c.sessions[sessionID]; ok && existing == ch {
				delete(c.sessions, sessionID)
				close(ch)
			}
			c.mu.Unlock()
		})
	}
}

func (c *conn) close() error {
	c.writeMu.Lock()
	c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	c.writeMu.Unlock()
	err := c.ws.Close()
	c.shutdown(err)
	return err
}
