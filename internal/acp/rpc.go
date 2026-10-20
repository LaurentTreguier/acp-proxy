package acp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// maxLineBytes caps a single ndjson line from the agent.
const maxLineBytes = 32 << 20

// clientHandler answers requests the agent sends to the client
// (session/request_permission, fs/*, terminal/*, ...).
type clientHandler struct {
	// onRequest returns the result for an agent->client request, or an
	// *RPCError. Unsupported methods must return an *RPCError with
	// code -32601.
	onRequest func(method string, params json.RawMessage) (any, error)
	// onUpdate is called for every session/update notification.
	onUpdate func(Update)
}

// conn is a JSON-RPC 2.0 connection over newline-delimited JSON, one side
// of the agent's stdio.
type conn struct {
	r      io.ReadCloser
	w      io.WriteCloser
	handler clientHandler

	writeMu sync.Mutex
	nextID  int64
	pendMu  sync.Mutex
	pend   map[int64]chan *response

	done    chan struct{}
	doneErr error
	doneOnce sync.Once
}

var errConnClosed = errors.New("acp: connection closed")

// newConn starts the read loop for a fresh connection.
func newConn(r io.ReadCloser, w io.WriteCloser, handler clientHandler) *conn {
	c := &conn{
		r:      r,
		w:      w,
		handler: handler,
		pend:   make(map[int64]chan *response),
		done:   make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// call sends a request and waits for the agent's response.
func (c *conn) call(method string, params any, result any) error {
	id, ch := c.register()
	if err := c.send(request{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		c.unregister(id)
		return err
	}
	resp, err := c.wait(id, ch)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("acp: %s: %w", method, resp.Error)
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("acp: decode %s result: %w", method, err)
		}
	}
	return nil
}

// notify sends a notification and does not wait for a response.
func (c *conn) notify(method string, params any) error {
	return c.send(request{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *conn) register() (int64, chan *response) {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	c.nextID++
	id := c.nextID
	ch := make(chan *response, 1)
	c.pend[id] = ch
	return id, ch
}

func (c *conn) unregister(id int64) {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	delete(c.pend, id)
}

func (c *conn) wait(id int64, ch chan *response) (*response, error) {
	defer c.unregister(id)
	select {
	case resp := <-ch:
		if resp == nil {
			return nil, fmt.Errorf("acp: agent connection lost: %w", c.doneErr)
		}
		return resp, nil
	case <-c.done:
		return nil, fmt.Errorf("acp: agent connection lost: %w", c.doneErr)
	}
}

func (c *conn) send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("acp: encode message: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, werr := c.w.Write(append(data, '\n'))
	if werr != nil {
		return fmt.Errorf("acp: write to agent: %w", werr)
	}
	return nil
}

func (c *conn) failAll(err error) {
	c.doneOnce.Do(func() {
		c.doneErr = err
		close(c.done)
	})
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	for id, ch := range c.pend {
		select {
		case ch <- nil:
		default:
		}
		delete(c.pend, id)
	}
}

func (c *conn) readLoop() {
	scanner := bufio.NewScanner(c.r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg inbound
		if err := json.Unmarshal(line, &msg); err != nil {
			// Ignore unparseable lines: agents may print banners on stdout.
			continue
		}
		switch {
		case msg.Method != "" && msg.ID != nil:
			c.handleRequest(msg)
		case msg.Method != "":
			c.handleNotification(msg)
		case msg.ID != nil:
			c.handleResponse(msg)
		}
	}
	c.failAll(fmt.Errorf("%w: %v", errConnClosed, scanner.Err()))
}

func (c *conn) handleResponse(msg inbound) {
	c.pendMu.Lock()
	ch, ok := c.pend[*msg.ID]
	delete(c.pend, *msg.ID)
	c.pendMu.Unlock()
	if !ok {
		return
	}
	resp := &response{JSONRPC: msg.JSONRPC, ID: *msg.ID, Result: msg.Result, Error: msg.Error}
	select {
	case ch <- resp:
	default:
	}
}

func (c *conn) handleNotification(msg inbound) {
	if msg.Method != "session/update" || c.handler.onUpdate == nil {
		return
	}
	var params sessionUpdateParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return
	}
	var body sessionUpdateBody
	if err := json.Unmarshal(params.Update, &body); err != nil {
		return
	}
	upd := Update{
		Kind:       body.SessionUpdate,
		MessageID:  body.MessageID,
		ToolCallID: body.ToolCallID,
		Status:    body.Status,
	}
	if body.Content != nil {
		upd.Text = body.Content.Text
	}
	c.handler.onUpdate(upd)
}

func (c *conn) handleRequest(msg inbound) {
	id := *msg.ID
	if c.handler.onRequest == nil {
		c.replyError(id, &RPCError{Code: codeMethodNotFound, Message: "method not supported"})
		return
	}
	result, err := c.handler.onRequest(msg.Method, msg.Params)
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			c.replyError(id, rpcErr)
			return
		}
		c.replyError(id, &RPCError{Code: codeInternalError, Message: err.Error()})
		return
	}
	c.send(response{JSONRPC: "2.0", ID: id, Result: mustMarshal(result)})
}

func (c *conn) replyError(id int64, rpcErr *RPCError) {
	c.send(response{JSONRPC: "2.0", ID: id, Error: rpcErr})
}

func mustMarshal(v any) json.RawMessage {
	if v == nil {
		return json.RawMessage("null")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return data
}
