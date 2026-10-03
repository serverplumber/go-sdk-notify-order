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
	cs, err := newTrackingClient(tr).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	report(t, callAndCount(t, cs, tr), tr)
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

// The proxy is an SDK server whose "work" tool calls the upstream's "work"
// through an SDK client, forwarding each upstream progress notification to its
// own caller under the tool handler's context.
func TestProxyForwardsProgressAfterResult(t *testing.T) {
	ctx := context.Background()

	upServerT, upClientT := mcp.NewInMemoryTransports()
	upSS, err := newUpstream().Connect(ctx, upServerT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upSS.Close()

	// forwarders maps a progress token to the in-flight proxy call it belongs to.
	var forwarders sync.Map // token -> func(*mcp.ProgressNotificationParams)
	var dropped atomic.Int64
	proxyClient := mcp.NewClient(&mcp.Implementation{Name: "proxy", Version: "v0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			if f, ok := forwarders.Load(req.Params.ProgressToken); ok {
				f.(func(*mcp.ProgressNotificationParams))(req.Params)
			} else {
				dropped.Add(1) // the proxy call has already returned
			}
		},
	})
	upCS, err := proxyClient.Connect(ctx, upClientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upCS.Close()

	proxy := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "v0"}, nil)
	mcp.AddTool(proxy, &mcp.Tool{Name: "work"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		token := req.Params.GetProgressToken()
		forwarders.Store(token, func(p *mcp.ProgressNotificationParams) {
			req.Session.NotifyProgress(ctx, p)
		})
		defer forwarders.Delete(token)

		params := &mcp.CallToolParams{Name: "work"}
		params.SetProgressToken(token)
		res, err := upCS.CallTool(ctx, params)
		return res, nil, err
	})

	downServerT, downClientT := mcp.NewInMemoryTransports()
	downSS, err := proxy.Connect(ctx, downServerT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer downSS.Close()

	tr := newTracker()
	cs, err := newTrackingClient(tr).Connect(ctx, downClientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	violations := callAndCount(t, cs, tr)
	t.Logf("proxy dropped %d upstream progress notifications that arrived after its tool handler returned", dropped.Load())
	report(t, violations, tr)
	if late := tr.lateCount(); late > 0 {
		t.Errorf("%d progress notifications reached the downstream client after its CallTool returned", late)
	}
}
