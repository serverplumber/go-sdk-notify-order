package notifyorder

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// wire wraps a client's transport and records the order in which the client
// reads messages off it, before the SDK's dispatch. It separates what the
// server sent from what the client's handlers saw: progress read after its
// call's response was sent late by the server, not reordered by the client.
//
// Only for in-memory transports: the Streamable HTTP client connection
// implements an unexported interface the SDK type-asserts for, which a
// wrapper would hide.
type wire struct {
	mcp.Transport

	mu       sync.Mutex
	tokenOf  map[string]string // request id -> progress token, from the tools/call written
	answered map[string]bool   // token -> its call's response has been read
	before   map[string]int    // token -> progress read before that response
	late     int               // progress read after its call's response
	total    int               // progress read at all
}

func newWire(t mcp.Transport) *wire {
	return &wire{
		Transport: t,
		tokenOf:   map[string]string{},
		answered:  map[string]bool{},
		before:    map[string]int{},
	}
}

func (w *wire) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := w.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &wireConn{Connection: c, w: w}, nil
}

type wireConn struct {
	mcp.Connection
	w *wire
}

func (c *wireConn) Write(ctx context.Context, m jsonrpc.Message) error {
	if r, ok := m.(*jsonrpc.Request); ok && r.Method == "tools/call" && r.ID.IsValid() {
		var p struct {
			Meta struct {
				ProgressToken any `json:"progressToken"`
			} `json:"_meta"`
		}
		if json.Unmarshal(r.Params, &p) == nil && p.Meta.ProgressToken != nil {
			c.w.mu.Lock()
			c.w.tokenOf[fmt.Sprint(r.ID.Raw())] = fmt.Sprint(p.Meta.ProgressToken)
			c.w.mu.Unlock()
		}
	}
	return c.Connection.Write(ctx, m)
}

func (c *wireConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	m, err := c.Connection.Read(ctx)
	if err == nil {
		c.w.read(m)
	}
	return m, err
}

func (w *wire) read(m jsonrpc.Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch m := m.(type) {
	case *jsonrpc.Request:
		if m.Method != "notifications/progress" {
			return
		}
		var p struct {
			ProgressToken any `json:"progressToken"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
		tok := fmt.Sprint(p.ProgressToken)
		w.total++
		if w.answered[tok] {
			w.late++
		} else {
			w.before[tok]++
		}
	case *jsonrpc.Response:
		if tok, ok := w.tokenOf[fmt.Sprint(m.ID.Raw())]; ok {
			w.answered[tok] = true
		}
	}
}

// counts reports, over the calls made so far, how many had fewer than want
// progress notifications read before their response, how many progress
// notifications were read after their call's response, and how many were
// read in all.
func (w *wire) counts(want int) (short, late, total int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for tok := range w.answered {
		if w.before[tok] < want {
			short++
		}
	}
	return short, w.late, w.total
}
