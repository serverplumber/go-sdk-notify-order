// Tests that a progress notification sent by a server before its response is
// delivered to the client's handler before CallTool returns. Each test asserts
// that property, so a failing run reproduces the bug.
package notifyorder

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const progressPerCall = 3

var calls = flag.Int("calls", envInt("NOTIFY_ORDER_CALLS", 1000), "CallTool calls per test (env NOTIFY_ORDER_CALLS)")

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return n
	}
	return def
}

// tracker counts progress per token, and how much arrived after the CallTool
// for that token had returned.
type tracker struct {
	mu       sync.Mutex
	seen     map[any]int
	returned map[any]bool
	late     int
}

func newTracker() *tracker {
	return &tracker{seen: map[any]int{}, returned: map[any]bool{}}
}

func (t *tracker) handler(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.returned[req.Params.ProgressToken] {
		t.late++
	}
	t.seen[req.Params.ProgressToken]++
}

// callReturned marks token's call as returned and reports the progress seen so far.
func (t *tracker) callReturned(token any) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.returned[token] = true
	return t.seen[token]
}

func (t *tracker) lateCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.late
}

// newUpstream returns a server whose "work" tool sends three progress
// notifications for the request's token, then returns at once.
func newUpstream() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "v0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "work"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		for i := range progressPerCall {
			err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: req.Params.GetProgressToken(),
				Progress:      float64(i + 1),
				Total:         progressPerCall,
			})
			if err != nil {
				return nil, nil, err
			}
		}
		return &mcp.CallToolResult{}, nil, nil
	})
	return s
}

// callAndCount makes *calls calls to "work", each with its own progress token,
// and returns how many returned before all their progress had been handled.
func callAndCount(t *testing.T, cs *mcp.ClientSession, tr *tracker) int {
	t.Helper()
	ctx := context.Background()
	violations := 0
	for i := range *calls {
		token := fmt.Sprintf("call-%d", i)
		params := &mcp.CallToolParams{Name: "work"}
		params.SetProgressToken(token)
		if _, err := cs.CallTool(ctx, params); err != nil {
			t.Fatalf("CallTool %d: %v", i, err)
		}
		if tr.callReturned(token) < progressPerCall {
			violations++
		}
	}
	return violations
}

func report(t *testing.T, violations int, tr *tracker) {
	t.Helper()
	t.Logf("%d calls; %d returned before all %d progress notifications were handled; %d notifications handled after their call returned",
		*calls, violations, progressPerCall, tr.lateCount())
	if violations > 0 {
		t.Errorf("CallTool returned before its progress notifications were handled in %d of %d calls", violations, *calls)
	}
}

func newTrackingClient(tr *tracker) *mcp.Client {
	return mcp.NewClient(&mcp.Implementation{Name: "client", Version: "v0"},
		&mcp.ClientOptions{ProgressNotificationHandler: tr.handler})
}

func TestProgressBeforeResult_InMemory(t *testing.T) {
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := newUpstream().Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	tr := newTracker()
	w := newWire(clientT)
	cs, err := newTrackingClient(tr).Connect(ctx, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	violations := callAndCount(t, cs, tr)

	// The server sent every call's progress before its response, and the
	// transport delivered it in that order; whatever the handlers saw late,
	// the client reordered.
	if short, late, _ := w.counts(progressPerCall); short > 0 || late > 0 {
		t.Fatalf("the transport itself delivered progress after its response (%d calls short, %d late); this test does not apply", short, late)
	}
	report(t, violations, tr)
}

func TestProgressBeforeResult_HTTP(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream()
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, nil))
	defer srv.Close()

	tr := newTracker()
	cs, err := newTrackingClient(tr).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	report(t, callAndCount(t, cs, tr), tr)
}

// A proxy: an SDK server whose "work" tool calls an upstream's "work" through
// an SDK client, forwarding the upstream's progress to its own caller.
//
// It has to stop forwarding a call's progress at some point, and the only
// moment the SDK gives it is the upstream CallTool returning -- by which time
// progress the upstream sent before its response may not have been handled.
// Stop there and that progress is lost; keep forwarding and it reaches the
// proxy's caller after the proxy has answered. The two tests below take one
// choice each.
type proxy struct {
	// keep has the proxy go on forwarding a call's progress after the call
	// has returned, rather than stop when it returns.
	keep bool

	forwarders sync.Map // progress token -> func(*mcp.ProgressNotificationParams)
	dropped    atomic.Int64
}

// connect stands up upstream -> proxy -> downstream over in-memory transports,
// and returns the downstream client's session, read through w.
func (p *proxy) connect(t *testing.T, w func(mcp.Transport) mcp.Transport) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	upServerT, upClientT := mcp.NewInMemoryTransports()
	upSS, err := newUpstream().Connect(ctx, upServerT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upSS.Close() })

	upCS, err := mcp.NewClient(&mcp.Implementation{Name: "proxy", Version: "v0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			if f, ok := p.forwarders.Load(req.Params.ProgressToken); ok {
				f.(func(*mcp.ProgressNotificationParams))(req.Params)
			} else {
				p.dropped.Add(1)
			}
		},
	}).Connect(ctx, upClientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upCS.Close() })

	srv := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "v0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "work"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		token := req.Params.GetProgressToken()
		if p.keep {
			// Outlives the call, so not under its context.
			p.forwarders.Store(token, func(n *mcp.ProgressNotificationParams) {
				_ = req.Session.NotifyProgress(context.Background(), n)
			})
		} else {
			p.forwarders.Store(token, func(n *mcp.ProgressNotificationParams) {
				_ = req.Session.NotifyProgress(ctx, n)
			})
			defer p.forwarders.Delete(token)
		}
		params := &mcp.CallToolParams{Name: "work"}
		params.SetProgressToken(token)
		res, err := upCS.CallTool(ctx, params)
		return res, nil, err
	})

	downServerT, downClientT := mcp.NewInMemoryTransports()
	downSS, err := srv.Connect(ctx, downServerT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { downSS.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "v0"}, nil).Connect(ctx, w(downClientT), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call makes *calls calls through the proxy, each with its own progress token.
func call(t *testing.T, cs *mcp.ClientSession) {
	t.Helper()
	for i := range *calls {
		params := &mcp.CallToolParams{Name: "work"}
		params.SetProgressToken(fmt.Sprintf("call-%d", i))
		if _, err := cs.CallTool(context.Background(), params); err != nil {
			t.Fatalf("CallTool %d: %v", i, err)
		}
	}
}

// A proxy that stops forwarding when the upstream call returns loses the
// progress the SDK had not yet handled.
func TestProxyLosesProgress(t *testing.T) {
	p := &proxy{}
	call(t, p.connect(t, func(tr mcp.Transport) mcp.Transport { return tr }))

	if n := p.dropped.Load(); n > 0 {
		t.Errorf("the proxy dropped %d of %d upstream progress notifications: they were handled after the upstream call returned", n, *calls*progressPerCall)
	}
}

// A proxy that keeps forwarding sends progress to its caller after it has
// answered, which 2025-11-25 basic/utilities/progress forbids: progress MUST
// only reference tokens of active requests, and MUST stop after completion.
// Read at the caller's transport, so this is what the proxy sent, not how the
// caller's own client ordered it.
func TestProxySendsProgressAfterResult(t *testing.T) {
	p := &proxy{keep: true}
	w := newWire(nil)
	call(t, p.connect(t, func(tr mcp.Transport) mcp.Transport { w.Transport = tr; return w }))

	// Every upstream notification is forwarded eventually; wait for the
	// stragglers so the count is complete rather than a lower bound.
	want := *calls * progressPerCall
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, late, total := w.counts(progressPerCall)
		if total >= want || time.Now().After(deadline) {
			t.Logf("%d calls; the caller read %d of %d progress notifications, %d after their call's response", *calls, total, want, late)
			if late > 0 {
				t.Errorf("the proxy sent %d progress notifications after answering the call they belong to", late)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
