package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	restate "github.com/restatedev/sdk-go"
)

// TestDiscoveryAuthHeader guards Admin-API discovery auth: fetchServices sends
// "Authorization: Bearer <token>" when DiscoverConfig.AuthToken is set (Restate Cloud
// requires it) and no auth header when it is empty.
func TestDiscoveryAuthHeader(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"services":[]}`))
	}))
	defer srv.Close()

	if _, err := fetchServices(context.Background(), srv.URL, "tok-123"); err != nil {
		t.Fatalf("fetchServices with token: %v", err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer tok-123")
	}
	if gotPath != "/services" {
		t.Fatalf("path = %q, want /services", gotPath)
	}

	gotAuth = "unset"
	if _, err := fetchServices(context.Background(), srv.URL, ""); err != nil {
		t.Fatalf("fetchServices without token: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty when no token", gotAuth)
	}
}

// TestNonSerializableReturnIsRecoverable guards F5: a program whose RETURN value cannot
// be JSON-serialized (BigInt, circular object) must surface as a NON-terminal program
// error mentioning serialization — the same recoverable shape as a thrown value — rather
// than the misleading terminal "program made no progress" error (which is what happened
// when the wrapper's success-path JSON.stringify threw uncaught and left __output unset).
func TestNonSerializableReturnIsRecoverable(t *testing.T) {
	eng, ctx := newTestEngine(t)
	for _, code := range []string{
		`return 1n;`,                          // BigInt
		`const o = {}; o.self = o; return o;`, // circular reference
	} {
		_, err := NewSandbox(eng, &testInvoker{}).RunProgram(ctx, code)
		if err == nil {
			t.Fatalf("code %q: expected an error, got nil", code)
		}
		if !strings.Contains(err.Error(), "JSON-serializable") {
			t.Fatalf("code %q: want a JSON-serializable error, got %v", code, err)
		}
		if strings.Contains(err.Error(), "no progress") {
			t.Fatalf("code %q: regressed to the misleading no-progress error: %v", code, err)
		}
		if restate.IsTerminalError(err) {
			t.Fatalf("code %q: error must be non-terminal so RunAgent feeds it back, got terminal: %v", code, err)
		}
	}
}

// TestExoticThrowIsRecoverable guards the hardened F5 wrapper: even a value whose error
// message cannot itself be stringified (a thrown null-proto object, a throwing `message`
// getter, a toJSON that throws an exotic value) must still yield a recoverable {s:2}
// program error — never leave __output unset and degrade to the misleading terminal
// "no progress" error. This exercises the constant-fallback path in wrapperPostJS.
func TestExoticThrowIsRecoverable(t *testing.T) {
	eng, ctx := newTestEngine(t)
	cases := []string{
		`return { toJSON() { throw Object.create(null); } };`,            // stringify throws a null-proto object
		`throw { get message() { throw 7; } };`,                          // thrown value's message getter throws
		`return { toJSON() { throw { get message() { throw 7; } }; } };`, // both the success and message paths throw
	}
	for _, code := range cases {
		_, err := NewSandbox(eng, &testInvoker{}).RunProgram(ctx, code)
		if err == nil {
			t.Fatalf("code %q: expected an error, got nil", code)
		}
		if strings.Contains(err.Error(), "no progress") {
			t.Fatalf("code %q: degraded to the misleading no-progress error: %v", code, err)
		}
		if restate.IsTerminalError(err) {
			t.Fatalf("code %q: want non-terminal, got terminal: %v", code, err)
		}
	}
}

// TestCloseTimeoutDoesNotCloseRuntime guards F6: if Close's context expires while a Run
// is still in flight, Close must return ctx.Err() WITHOUT closing the runtime (closing it
// would pull the instance out from under the active guest call), stay in the closing
// state (rejecting new Runs), let the in-flight Run finish cleanly, and allow a later
// Close with a fresh context to complete shutdown.
func TestCloseTimeoutDoesNotCloseRuntime(t *testing.T) {
	ctx := context.Background()
	eng, err := NewEngine(ctx, guestWasm)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	inv := &testInvoker{}
	inv.add("block", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		close(started)
		<-release
		return json.RawMessage(`{"ok":true}`), nil
	})
	runErr := make(chan error, 1)
	go func() {
		_, e := NewSandbox(eng, inv).RunProgram(ctx, `await block({}); return {done:true, answer:"x"};`)
		runErr <- e
	}()
	<-started // Run is in-flight, blocked inside the tool

	// Close with a short deadline: must time out and return DeadlineExceeded, NOT close.
	tctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if e := eng.Close(tctx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("timed-out Close: want context.DeadlineExceeded, got %v", e)
	}
	// New Runs are rejected once closing began.
	if _, e := NewSandbox(eng, inv).RunProgram(ctx, `return 1;`); !errors.Is(e, errEngineClosed) {
		t.Fatalf("during closing: want errEngineClosed, got %v", e)
	}
	// The in-flight Run must still be usable — if the runtime had been closed under it,
	// the post-release guest.resolve would fail.
	close(release)
	if e := <-runErr; e != nil {
		t.Fatalf("in-flight Run failed (runtime closed under it?): %v", e)
	}
	// A second Close with a live context now finishes shutdown cleanly.
	if e := eng.Close(context.Background()); e != nil {
		t.Fatalf("second Close after drain: %v", e)
	}
}

// TestResolveToolSet guards F4: static tools are validated strictly (a bad or duplicate
// name is a terminal config error), while discovered tools (untrusted annotation
// metadata) that are invalid, reserved, or collide are dropped-and-logged so one bad
// service can't brick the session. First-registered wins a name; static wins over
// discovered.
func TestResolveToolSet(t *testing.T) {
	mk := func(name string) Tool { return Tool{Name: name} }
	noop := func(string, ...any) {}

	// A duplicate static name is a terminal error.
	if _, err := resolveToolSet([]Tool{mk("a"), mk("a")}, nil, noop); err == nil || !restate.IsTerminalError(err) {
		t.Fatalf("duplicate static: want terminal error, got %v", err)
	}

	// Empty, reserved (incl. String — the wrapper stringifies errors with it), and
	// non-JS-safe static names are terminal errors.
	for _, bad := range []string{"", "JSON", "String", "Promise", "__hostCall", "has space", "1bad", "a-b"} {
		if _, err := resolveToolSet([]Tool{mk(bad)}, nil, noop); err == nil {
			t.Fatalf("static name %q: want error, got nil", bad)
		}
	}

	// A valid static set passes unchanged.
	out, err := resolveToolSet([]Tool{mk("reserve_stock"), mk("charge_payment")}, nil, noop)
	if err != nil || len(out) != 2 {
		t.Fatalf("valid static: err=%v len=%d", err, len(out))
	}

	// A discovered tool colliding with a static one is dropped (static kept), logged.
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	out, err = resolveToolSet([]Tool{mk("a")}, []Tool{mk("a"), mk("b")}, logf)
	if err != nil {
		t.Fatalf("collision should drop, not error: %v", err)
	}
	if len(out) != 2 || out[0].Name != "a" || out[1].Name != "b" {
		t.Fatalf("want [a(static) b(discovered)], got %+v", out)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 drop log, got %v", logs)
	}

	// Discovered reserved/invalid names are dropped, not fatal.
	out, err = resolveToolSet(nil, []Tool{mk("JSON"), mk("__x"), mk("ok")}, noop)
	if err != nil {
		t.Fatalf("discovered invalid should drop, not error: %v", err)
	}
	if len(out) != 1 || out[0].Name != "ok" {
		t.Fatalf("want [ok], got %+v", out)
	}

	// Two discovered tools with the same name: the second is dropped.
	out, _ = resolveToolSet(nil, []Tool{mk("dup"), mk("dup")}, noop)
	if len(out) != 1 {
		t.Fatalf("want 1 after discovered dedupe, got %d", len(out))
	}
}

// TestNewServiceRejectsBadStaticTool guards F4 at boot: a reserved static tool name
// fails NewService rather than surfacing on the first Ask.
func TestNewServiceRejectsBadStaticTool(t *testing.T) {
	if _, err := NewService(context.Background(), Config{Tools: []Tool{{Name: "JSON"}}}); err == nil {
		t.Fatal("expected NewService to reject the reserved tool name \"JSON\"")
	}
}

// TestExecutionBudgetInterruptsInfiniteLoop guards F2: a program that never yields to the
// host — a synchronous infinite loop, or a self-re-enqueuing microtask — is stopped by
// the guest's deterministic execution budget and surfaces as a recoverable program error,
// rather than pinning the worker until external cancellation.
func TestExecutionBudgetInterruptsInfiniteLoop(t *testing.T) {
	eng, ctx := newTestEngine(t)
	cases := []struct{ name, code string }{
		// A synchronous infinite loop at the top level (tripped during initial eval).
		{"sync-loop", `while (true) {}`},
		// An infinite loop inside a microtask (tripped during the drive/settle phase, not
		// the initial eval) — the budget must span job draining too.
		{"microtask-loop", `await Promise.resolve().then(function(){ while (true) {} });`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := NewSandbox(eng, &testInvoker{}).RunProgram(ctx, tc.code)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected an execution-budget error, got nil")
				}
				if !strings.Contains(err.Error(), "execution budget") {
					t.Fatalf("expected execution-budget error, got %v", err)
				}
				if restate.IsTerminalError(err) {
					t.Fatalf("budget error must be non-terminal, got terminal: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("infinite program was not interrupted within 20s")
			}
		})
	}
}

// TestExecutionBudgetAllowsFiniteWork guards the other side of F2: a large-but-finite
// computation completes under the budget rather than being falsely interrupted.
func TestExecutionBudgetAllowsFiniteWork(t *testing.T) {
	eng, ctx := newTestEngine(t)
	got, err := NewSandbox(eng, &testInvoker{}).RunProgram(ctx,
		`let s = 0; for (let i = 0; i < 2000000; i++) s += i; return s;`)
	if err != nil {
		t.Fatalf("finite loop should complete under budget, got %v", err)
	}
	if got != "1999999000000" {
		t.Fatalf("unexpected sum: %s", got)
	}
}
