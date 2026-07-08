package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	restate "github.com/restatedev/sdk-go"
)

func newTestEngine(t *testing.T) (*Engine, context.Context) {
	t.Helper()
	ctx := context.Background()
	eng, err := NewEngine(ctx, guestWasm)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(ctx) })
	return eng, ctx
}

// TestEvalAndReturn: a tool-free program runs to completion in one round (no
// frontier) and RunProgram returns its value as JSON.
func TestEvalAndReturn(t *testing.T) {
	eng, ctx := newTestEngine(t)
	for _, tc := range []struct{ name, code, want string }{
		{"json_object", `return {a:1,b:2};`, `{"a":1,"b":2}`},
		{"string", `return "hi";`, `"hi"`},
		{"number", `return 6*7;`, `42`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewSandbox(eng, &testInvoker{}).RunProgram(ctx, tc.code)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPromiseAllFrontier: a Promise.all of two tools is collected as one frontier
// and both resolve; the results feed back on re-execution.
func TestPromiseAllFrontier(t *testing.T) {
	eng, ctx := newTestEngine(t)
	inv := &testInvoker{}
	inv.add("echo", func(_ context.Context, arg json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(fmt.Sprintf(`{"v":%s}`, arg)), nil // arg is a JSON value
	})
	got, err := NewSandbox(eng, inv).RunProgram(ctx,
		`const [a,b] = await Promise.all([echo("x"), echo("y")]); return a.v + "|" + b.v;`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != `"x|y"` {
		t.Fatalf("got %q, want \"x|y\"", got)
	}
}

// TestLargeFrontier: a Promise.all of 5000 tool calls is collected as one frontier
// (a plain JS array — no fixed cap) and all resolve.
func TestLargeFrontier(t *testing.T) {
	eng, ctx := newTestEngine(t)
	const n = 5000
	inv := &testInvoker{}
	inv.add("echo", func(_ context.Context, arg json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(fmt.Sprintf(`{"v":%s}`, arg)), nil // arg is a number
	})
	code := fmt.Sprintf(`
		const ps = [];
		for (let i = 0; i < %d; i++) ps.push(echo(i));
		const r = await Promise.all(ps);
		let sum = 0; for (const x of r) sum += x.v;
		return sum;`, n)
	got, err := NewSandbox(eng, inv).RunProgram(ctx, code)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := fmt.Sprintf("%d", n*(n-1)/2); got != want {
		t.Fatalf("got %q, want %q (all %d calls must resolve)", got, want, n)
	}
}

// TestPromiseRaceFirstWins: Promise.race settles with the FIRST delivered completion
// and the program proceeds WITHOUT the loser — the capability the live model exists
// for (a timeout is race([work, sleep])). The test invoker delivers the lowest handle
// first, so `a` (handle 0) wins and `b` is abandoned when the program returns.
func TestPromiseRaceFirstWins(t *testing.T) {
	eng, ctx := newTestEngine(t)
	inv := &testInvoker{}
	inv.add("a", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"A"`), nil
	})
	inv.add("b", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"B"`), nil
	})
	got, err := NewSandbox(eng, inv).RunProgram(ctx, `const r = await Promise.race([a(), b()]); return r;`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != `"A"` {
		t.Fatalf("race should settle with the first completion (a); got %q", got)
	}
}

// TestProgramThrow: a thrown error surfaces as a non-nil RunProgram error.
func TestProgramThrow(t *testing.T) {
	eng, ctx := newTestEngine(t)
	_, err := NewSandbox(eng, &testInvoker{}).RunProgram(ctx, `throw new Error("boom");`)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want error containing boom, got %v", err)
	}
}

// TestGracefulShutdownWaitsForInflight: Close must wait for an in-flight Run to
// finish (so runtime.Close can't close an instance under an active guest call), and
// reject Runs started after Close begins.
func TestGracefulShutdownWaitsForInflight(t *testing.T) {
	ctx := context.Background()
	eng, err := NewEngine(ctx, guestWasm)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	inv := &testInvoker{}
	inv.add("block", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		close(started) // signal we're mid-Run (tool invoked, inside engine.Run)
		<-release      // block until the test lets go
		return json.RawMessage(`{"ok":true}`), nil
	})

	runErr := make(chan error, 1)
	go func() {
		_, e := NewSandbox(eng, inv).RunProgram(ctx, `await block({}); return {done:true, answer:"x"};`)
		runErr <- e
	}()
	<-started // the Run is in-flight, blocked inside the tool

	closeErr := make(chan error, 1)
	go func() { closeErr <- eng.Close(context.Background()) }()
	select {
	case <-closeErr:
		t.Fatal("Close returned before the in-flight Run finished")
	case <-time.After(200 * time.Millisecond):
	}

	close(release) // let the Run finish
	if e := <-runErr; e != nil {
		t.Fatalf("in-flight Run failed (instance closed under it?): %v", e)
	}
	select {
	case e := <-closeErr:
		if e != nil {
			t.Fatalf("Close: %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the in-flight Run finished")
	}

	if _, e := NewSandbox(eng, inv).RunProgram(ctx, `return 1;`); !errors.Is(e, errEngineClosed) {
		t.Fatalf("expected errEngineClosed after Close, got %v", e)
	}
}

// TestConcurrentRunsAreIsolated: many concurrent Runs over ONE shared Engine (as
// concurrent Agent/<session>/Ask invocations do) must not share QuickJS state.
// Each goroutine runs a program that awaits a tool echoing its own unique value and
// returns a value derived from it; if the pool ever handed one wasm instance to two
// goroutines, or the guest state weren't per-instance, results would cross-
// contaminate (or -race would fire on the shared Engine/pool). 128 goroutines >
// poolMaxIdle (32), so instances churn through the pool under contention.
func TestConcurrentRunsAreIsolated(t *testing.T) {
	eng, ctx := newTestEngine(t) // shared across all goroutines
	const n = 128
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			inv := &testInvoker{}
			inv.add("echo", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(fmt.Sprintf(`{"v":%d}`, k)), nil
			})
			// await forces a full host round-trip (pending → resolve → microtasks),
			// exercising per-instance state concurrently.
			out, err := NewSandbox(eng, inv).RunProgram(ctx, `const r = await echo({}); return r.v * 1000 + 7;`)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", k, err)
				return
			}
			if want := fmt.Sprintf("%d", k*1000+7); out != want {
				errs <- fmt.Errorf("goroutine %d: got %q want %q (cross-instance state contamination)", k, out, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// testInvoker is a minimal in-memory Invoker for sandbox/loop tests. Tools run
// SYNCHRONOUSLY at Start (the real invoker submits durable futures instead); each
// settled result is buffered by handle and delivered by Next in ascending-handle
// order, which is deterministic.
type testInvoker struct {
	tools map[string]func(context.Context, json.RawMessage) (json.RawMessage, error)
	specs []ToolSpec
	done  map[int]StepResult
}

func (m *testInvoker) add(name string, fn func(context.Context, json.RawMessage) (json.RawMessage, error)) {
	m.addWithSchema(name, "", "", fn)
}
func (m *testInvoker) addWithSchema(name, desc, params string, fn func(context.Context, json.RawMessage) (json.RawMessage, error)) {
	if m.tools == nil {
		m.tools = map[string]func(context.Context, json.RawMessage) (json.RawMessage, error){}
	}
	m.tools[name] = fn
	spec := ToolSpec{Name: name, Description: desc}
	if params != "" {
		spec.Params = json.RawMessage(params)
	}
	m.specs = append(m.specs, spec)
}
func (m *testInvoker) Tools() []ToolSpec { return m.specs }
func (m *testInvoker) Reset()            { m.done = nil }
func (m *testInvoker) Start(calls []ToolCall) {
	if m.done == nil {
		m.done = map[int]StepResult{}
	}
	for _, c := range calls {
		m.done[c.Handle] = runTestTool(m.tools[c.Tool], c)
	}
}
func (m *testInvoker) Pending() int                             { return len(m.done) }
func (m *testInvoker) Next(context.Context) (StepResult, error) { return popLowest(m.done), nil }

// runTestTool executes an in-memory test tool and returns its settled StepResult,
// mirroring the validation the real invoker's Next does (unknown tool / error /
// empty → null / invalid JSON → tool-named error).
func runTestTool(fn func(context.Context, json.RawMessage) (json.RawMessage, error), c ToolCall) StepResult {
	if fn == nil {
		return StepResult{Handle: c.Handle, ErrMsg: fmt.Sprintf("unknown tool %q", c.Tool), IsErr: true}
	}
	v, err := fn(context.Background(), c.Arg)
	switch {
	case err != nil:
		return StepResult{Handle: c.Handle, ErrMsg: err.Error(), IsErr: true}
	case len(v) == 0:
		return StepResult{Handle: c.Handle, Value: json.RawMessage("null")}
	case !json.Valid(v):
		return StepResult{Handle: c.Handle, ErrMsg: fmt.Sprintf("tool %q returned invalid JSON", c.Tool), IsErr: true}
	default:
		return StepResult{Handle: c.Handle, Value: v}
	}
}

// popLowest removes and returns the lowest-handle buffered result (deterministic).
func popLowest(m map[int]StepResult) StepResult {
	h := -1
	for k := range m {
		if h < 0 || k < h {
			h = k
		}
	}
	res := m[h]
	delete(m, h)
	return res
}

// scriptModel returns pre-scripted decisions; each step may inspect the convo.
type scriptModel struct {
	steps []func(*Conversation) Decision
	i     int
}

func (m *scriptModel) Decide(_ context.Context, c *Conversation) (Decision, error) {
	d := m.steps[m.i](c)
	m.i++
	return d, nil
}

// TestSandboxRunsProgramWithTools: a program awaits a registered tool and returns
// an observation built from its result.
func TestSandboxRunsProgramWithTools(t *testing.T) {
	eng, ctx := newTestEngine(t)
	inv := &testInvoker{}
	inv.add("calc", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"value":42}`), nil
	})
	// The program returns a plain value; the sandbox JSON-encodes it.
	got, err := NewSandbox(eng, inv).RunProgram(ctx,
		`const c = await calc({expr:"6*7"}); return {answer: c.value};`)
	if err != nil {
		t.Fatalf("run program: %v", err)
	}
	if got != `{"answer":42}` {
		t.Fatalf("got %q", got)
	}
}

// TestOnlyRegisteredTools: the guest exposes only the generic __hostCall bridge;
// the sandbox defines exactly the registered tool names on top of it — no
// hardcoded/phantom tool globals exist.
func TestOnlyRegisteredTools(t *testing.T) {
	eng, ctx := newTestEngine(t)
	inv := &testInvoker{}
	inv.add("calc", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"value":1}`), nil
	})
	got, err := NewSandbox(eng, inv).RunProgram(ctx,
		`return [typeof __hostCall, typeof calc, typeof webSearch];`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != `["function","function","undefined"]` {
		t.Fatalf("expected bridge+calc present, webSearch absent; got %s", got)
	}
}

// TestGoLoop: the Go loop drives a scripted model that ALWAYS writes programs —
// round 1 gathers a tool result (done:false), round 2 finishes (done:true) after
// seeing the observation.
func TestGoLoop(t *testing.T) {
	eng, ctx := newTestEngine(t)
	inv := &testInvoker{}
	inv.add("calc", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"value":42}`), nil
	})
	model := &scriptModel{steps: []func(*Conversation) Decision{
		func(_ *Conversation) Decision {
			return Decision{Code: `const c = await calc({expr:"6*7"}); return {done:false, value: c.value};`}
		},
		func(c *Conversation) Decision {
			var obs struct {
				Value int `json:"value"`
			}
			_ = json.Unmarshal([]byte(c.Turns[len(c.Turns)-1].Content), &obs)
			return Decision{Code: fmt.Sprintf(`return {done:true, answer:%q};`, intToAnswer(obs.Value))}
		},
	}}
	ans, err := RunAgent(ctx, NewSandbox(eng, inv), model, NewConversation("6*7?"), 5, nil)
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if ans != "the answer is 42" {
		t.Fatalf("got %q", ans)
	}
}

// TestLoopRecoversFromProgramError: a failing program yields an ERROR observation
// that the model reacts to on the next round ("loop until the tools succeed").
func TestLoopRecoversFromProgramError(t *testing.T) {
	eng, ctx := newTestEngine(t)
	inv := &testInvoker{}
	model := &scriptModel{steps: []func(*Conversation) Decision{
		// Round 1: buggy program (throws).
		func(_ *Conversation) Decision {
			return Decision{Code: `throw new Error("oops bad code");`}
		},
		// Round 2: finish, but only if we actually saw the error observation.
		func(c *Conversation) Decision {
			last := c.Turns[len(c.Turns)-1]
			if last.Role != "observation" || !strings.HasPrefix(last.Content, "ERROR:") {
				return Decision{Code: `return {done:true, answer:"no-error-seen"};`}
			}
			return Decision{Code: `return {done:true, answer:"recovered"};`}
		},
	}}
	ans, err := RunAgent(ctx, NewSandbox(eng, inv), model, NewConversation("task"), 5, nil)
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if ans != "recovered" {
		t.Fatalf("expected recovery to 'recovered', got %q", ans)
	}
}

// TestDeterminism: with the same seed + frozen clock, a program's Math.random,
// Date.now, and new Date() reproduce identically across fresh sandboxes (i.e. a
// replay is stable); a different seed changes the randomness.
func TestDeterminism(t *testing.T) {
	eng, ctx := newTestEngine(t)
	const prog = `return [Math.random(), Math.random(), Date.now(), new Date().getTime()];`
	run := func(seed, now int64) string {
		sb := NewSandbox(eng, &testInvoker{})
		sb.SetDeterminism(seed, now)
		out, err := sb.RunProgram(ctx, prog)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return out
	}
	a := run(12345, 1700000000000)
	if b := run(12345, 1700000000000); a != b {
		t.Fatalf("same seed+clock must reproduce:\n a=%s\n b=%s", a, b)
	}
	// Clock frozen to nowMillis for both Date.now() and new Date().
	if !strings.Contains(a, "1700000000000,1700000000000") {
		t.Fatalf("clock not frozen to nowMillis: %s", a)
	}
	if c := run(999, 1700000000000); a == c {
		t.Fatalf("different seed should change Math.random: %s", a)
	}
}

// TestDateFreezeNotEscapableViaConstructor: reaching Date through .constructor must
// still yield the frozen `now`, not the pinned WASI clock, and `instanceof Date`
// must still hold. nowMillis is deliberately != detFixedEpochSec*1000 so the two
// are distinguishable.
func TestDateFreezeNotEscapableViaConstructor(t *testing.T) {
	eng, ctx := newTestEngine(t)
	sb := NewSandbox(eng, &testInvoker{})
	const now = 1800000000000 // 2027; differs from the pinned WASI constant (2023)
	sb.SetDeterminism(1, now)
	got, err := sb.RunProgram(ctx, `
		const viaCtor = new (new Date().constructor)().getTime();
		const direct = new Date().getTime();
		const isInst = (new Date()) instanceof Date;
		return {viaCtor, direct, isInst};`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != `{"viaCtor":1800000000000,"direct":1800000000000,"isInst":true}` {
		t.Fatalf("Date freeze escaped via .constructor, or instanceof broke: %s", got)
	}
}

// TestToolSchemaSurfaced: a tool's JSON Schema and description appear in the
// system prompt handed to the model.
func TestToolSchemaSurfaced(t *testing.T) {
	inv := &testInvoker{}
	inv.addWithSchema("calc", "evaluate math",
		`{"type":"object","properties":{"expr":{"type":"string"}},"required":["expr"]}`, nil)
	prompt := BuildSystemPrompt(inv.Tools())
	for _, want := range []string{"calc", "evaluate math", `"expr"`, `"required":["expr"]`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("system prompt missing %q; got:\n%s", want, prompt)
		}
	}
}

// TestToolShapeAndSchemas: NewTool is a leaf (submit set, seqHandler nil), NewSeqTool
// is a sequence (seqHandler set, submit nil), and both reflect arg + result schemas
// from their type params.
func TestToolShapeAndSchemas(t *testing.T) {
	type in struct {
		X int `json:"x"`
	}
	type out struct {
		Y string `json:"y"`
	}
	leaf := NewTool("leaf", "d", func(_ restate.Context, _ in) (Future[out], error) { return Future[out]{}, nil })
	if leaf.submit == nil || leaf.seqHandler != nil {
		t.Fatalf("NewTool must set submit (leaf), not seqHandler")
	}
	seq := NewSeqTool("seq", "d", func(_ restate.Context, _ in) (out, error) { return out{}, nil })
	if seq.seqHandler == nil || seq.submit != nil {
		t.Fatalf("NewSeqTool must set seqHandler (sequence), not submit")
	}
	for _, tl := range []Tool{leaf, seq} {
		if !strings.Contains(string(tl.Params), `"x"`) {
			t.Fatalf("%s: arg schema missing x: %s", tl.Name, tl.Params)
		}
		if !strings.Contains(string(tl.Result), `"y"`) {
			t.Fatalf("%s: result schema missing y: %s", tl.Name, tl.Result)
		}
	}
}

// TestEmptyFutureRejected: a leaf tool that returns a zero-value Future (nil sel)
// is rejected with a clear error by submit, instead of nil-panicking the batch
// driver (which, being deterministic, would retry forever). The guard path never
// touches the context, so a nil ctx is fine here.
func TestEmptyFutureRejected(t *testing.T) {
	type in struct{}
	type out struct{}
	tl := NewTool("empty", "d", func(_ restate.Context, _ in) (Future[out], error) {
		return Future[out]{}, nil // author bug: not built via Run/Call/Timer/…
	})
	if _, err := tl.submit(nil, json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected submit to reject a zero-value Future, got nil error")
	}
}

// TestResultSchemaSurfaced: a tool's RETURN schema (not just its args) reaches the
// system prompt, so the model knows the shape a tool resolves to.
func TestResultSchemaSurfaced(t *testing.T) {
	specs := []ToolSpec{{
		Name:        "weather",
		Description: "get weather",
		Params:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		Result:      json.RawMessage(`{"type":"object","properties":{"tempC":{"type":"number"}}}`),
	}}
	prompt := BuildSystemPrompt(specs)
	for _, want := range []string{"returns:", `"tempC"`, `"city"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("system prompt missing %q; got:\n%s", want, prompt)
		}
	}
}

// neverDoneModel always emits a program that never signals done — for testing
// the round budget.
type neverDoneModel struct{}

func (neverDoneModel) Decide(_ context.Context, _ *Conversation) (Decision, error) {
	return Decision{Code: `return {done:false};`}, nil
}

// TestMaxRoundsTerminal: exhausting the round budget yields ErrMaxRounds (which
// Solve maps to a terminal error, so Restate doesn't retry the deterministic
// give-up forever).
func TestMaxRoundsTerminal(t *testing.T) {
	eng, ctx := newTestEngine(t)
	_, err := RunAgent(ctx, NewSandbox(eng, &testInvoker{}), neverDoneModel{}, NewConversation("t"), 3, nil)
	if !errors.Is(err, ErrMaxRounds) {
		t.Fatalf("expected ErrMaxRounds, got %v", err)
	}
}

// TestEmptyProgramFedBack: an empty program is a recoverable observation, not a
// fatal (retry-forever) error — the model corrects on the next round.
func TestEmptyProgramFedBack(t *testing.T) {
	eng, ctx := newTestEngine(t)
	model := &scriptModel{steps: []func(*Conversation) Decision{
		func(_ *Conversation) Decision { return Decision{Code: ""} },
		func(c *Conversation) Decision {
			last := c.Turns[len(c.Turns)-1]
			if last.Role != "observation" || !strings.HasPrefix(last.Content, "ERROR:") {
				return Decision{Code: `return {done:true, answer:"no-feedback"};`}
			}
			return Decision{Code: `return {done:true, answer:"ok"};`}
		},
	}}
	ans, err := RunAgent(ctx, NewSandbox(eng, &testInvoker{}), model, NewConversation("t"), 5, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if ans != "ok" {
		t.Fatalf("expected recovery after empty program, got %q", ans)
	}
}

// recordingInvoker records every (tool, arg) dispatched, to compare across runs.
type recordingInvoker struct {
	inner Invoker
	calls *[]string
}

func (r *recordingInvoker) Tools() []ToolSpec { return r.inner.Tools() }
func (r *recordingInvoker) Reset()            { r.inner.Reset() }
func (r *recordingInvoker) Start(calls []ToolCall) {
	for _, c := range calls {
		*r.calls = append(*r.calls, c.Tool+"("+string(c.Arg)+")")
	}
	r.inner.Start(calls)
}
func (r *recordingInvoker) Pending() int                                 { return r.inner.Pending() }
func (r *recordingInvoker) Next(ctx context.Context) (StepResult, error) { return r.inner.Next(ctx) }

// TestReplayDeterminism is the core durability guarantee at the integration
// level: two independent runs (fresh QuickJS instances — as after a crash+replay)
// with the same seed and frozen clock produce the IDENTICAL sequence of tool
// calls, even though the program derives its tool argument from Math.random() and
// Date.now(). If determinism weren't enforced, the two runs would compute
// different args and the journal would diverge.
func TestReplayDeterminism(t *testing.T) {
	eng, ctx := newTestEngine(t)
	newModel := func() *scriptModel {
		return &scriptModel{steps: []func(*Conversation) Decision{
			func(_ *Conversation) Decision {
				return Decision{Code: `
					const r = Math.floor(Math.random() * 1000);
					const t = Date.now() % 100;
					const c = await calc({ expr: r + "+" + t });
					return { done: false, sum: c.value };`}
			},
			func(c *Conversation) Decision {
				var obs struct {
					Sum int `json:"sum"`
				}
				_ = json.Unmarshal([]byte(c.Turns[len(c.Turns)-1].Content), &obs)
				return Decision{Code: fmt.Sprintf(`return {done:true, answer:"%d"};`, obs.Sum)}
			},
		}}
	}
	run := func() (string, string) {
		base := &testInvoker{}
		base.add("calc", testCalc)
		var calls []string
		sb := NewSandbox(eng, &recordingInvoker{inner: base, calls: &calls})
		sb.SetDeterminism(42, 1700000000000)
		ans, err := RunAgent(ctx, sb, newModel(), NewConversation("go"), 5, nil)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return strings.Join(calls, "|"), ans
	}

	calls1, a1 := run()
	calls2, a2 := run()
	if calls1 == "" {
		t.Fatal("expected at least one recorded tool call")
	}
	if calls1 != calls2 {
		t.Fatalf("tool-call sequence diverged across identical runs:\n  %s\n  %s", calls1, calls2)
	}
	if a1 != a2 {
		t.Fatalf("answer diverged across identical runs: %q vs %q", a1, a2)
	}
}

// TestParallelTools: a single program firing two tool calls via Promise.all is
// resolved in one batch through the Agent's dispatch (not just the raw engine).
func TestParallelTools(t *testing.T) {
	eng, ctx := newTestEngine(t)
	inv := &testInvoker{}
	inv.add("calc", testCalc)
	sb := NewSandbox(eng, inv)

	got, err := sb.RunProgram(ctx,
		`const [a,b] = await Promise.all([calc({expr:"2*3"}), calc({expr:"4*5"})]); return a.value + b.value;`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "26" { // 6 + 20
		t.Fatalf("parallel tools = %q, want 26", got)
	}
}

// batchMock records the largest batch (single Start call) it saw — a Promise.all of
// N tool calls is delivered to Start as one batch of N.
type batchMock struct {
	specs    []ToolSpec
	maxBatch int
	fn       func(string, json.RawMessage) (json.RawMessage, error)
	done     map[int]StepResult
}

func (b *batchMock) Tools() []ToolSpec { return b.specs }
func (b *batchMock) Reset()            { b.done = nil }
func (b *batchMock) Start(calls []ToolCall) {
	if len(calls) > b.maxBatch {
		b.maxBatch = len(calls)
	}
	if b.done == nil {
		b.done = map[int]StepResult{}
	}
	for _, c := range calls {
		v, err := b.fn(c.Tool, c.Arg)
		if err != nil {
			b.done[c.Handle] = StepResult{Handle: c.Handle, ErrMsg: err.Error(), IsErr: true}
		} else {
			b.done[c.Handle] = StepResult{Handle: c.Handle, Value: v}
		}
	}
}
func (b *batchMock) Pending() int                             { return len(b.done) }
func (b *batchMock) Next(context.Context) (StepResult, error) { return popLowest(b.done), nil }

// TestBatchInvoker: a Promise.all of N tool calls is delivered to InvokeBatch as ONE
// frontier of N — the hook a durable invoker uses to run them in parallel
// (restate.RunAsync). Verifies the wiring offline.
func TestBatchInvoker(t *testing.T) {
	eng, ctx := newTestEngine(t)
	m := &batchMock{
		specs: []ToolSpec{{Name: "calc"}},
		fn: func(_ string, arg json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Expr string `json:"expr"`
			}
			_ = json.Unmarshal(arg, &in)
			return json.RawMessage(fmt.Sprintf(`{"value":%d}`, evalExpr(in.Expr))), nil
		},
	}
	got, err := NewSandbox(eng, m).RunProgram(ctx,
		`const r = await Promise.all([calc({expr:"1+1"}), calc({expr:"2+2"}), calc({expr:"3+3"})]); return r[0].value + r[1].value + r[2].value;`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "12" { // 2 + 4 + 6
		t.Fatalf("got %q, want 12", got)
	}
	if m.maxBatch != 3 {
		t.Fatalf("expected one batch of 3, Start saw max %d", m.maxBatch)
	}
}

// TestSession: history carries across two Asks — a follow-up message builds on
// the prior answer (proving multi-turn memory the Virtual Object persists).
func TestSession(t *testing.T) {
	eng, ctx := newTestEngine(t)
	inv := &testInvoker{}
	inv.add("calc", testCalc)
	sb := NewSandbox(eng, inv)

	var history []Turn
	run := func(msg string) string {
		convo := &Conversation{Turns: append(history, Turn{Role: RoleUser, Content: msg})}
		ans, err := RunAgent(ctx, sb, demoModel{}, convo, 5, nil)
		if err != nil {
			t.Fatalf("run %q: %v", msg, err)
		}
		history = convo.Turns
		return ans
	}

	if a := run("what is 6 times 7?"); !strings.Contains(a, "42") {
		t.Fatalf("first answer %q, want it to contain 42", a)
	}
	if a := run("add 1 to that"); !strings.Contains(a, "43") {
		t.Fatalf("follow-up should build on 42 → 43, got %q", a)
	}
	users := 0
	for _, tn := range history {
		if tn.Role == RoleUser {
			users++
		}
	}
	if users != 2 {
		t.Fatalf("expected 2 user turns in session history, got %d", users)
	}
}

// TestPoolReuseIsolation: when an instance is reused from the pool, one program's
// global pollution must NOT be visible to the next. Each program gets a FRESH QuickJS
// context at start(), so isolation is inherent; this guards the correctness/security
// property pooling depends on.
func TestPoolReuseIsolation(t *testing.T) {
	eng, ctx := newTestEngine(t)
	sb := NewSandbox(eng, &testInvoker{})
	// Program A pollutes the global object and a built-in prototype, then returns
	// (so its instance is released back to the pool).
	if _, err := sb.RunProgram(ctx, `globalThis.__leak = 123; Array.prototype.__pwn = 1; return {done:false};`); err != nil {
		t.Fatalf("run A: %v", err)
	}
	// Program B reuses A's instance; it must see a pristine global.
	got, err := sb.RunProgram(ctx, `return [typeof globalThis.__leak, typeof ([].__pwn)];`)
	if err != nil {
		t.Fatalf("run B: %v", err)
	}
	if got != `["undefined","undefined"]` {
		t.Fatalf("state leaked across reused instance: %s", got)
	}
}

// TestThrowingMicrotaskContained: a program with an unhandled throwing microtask
// must not crash the guest (the QuickJS teardown handles it — the guest is built
// with NDEBUG), and a subsequent run on the same pooled instance must be unaffected.
// Each program gets a fresh context at start(), so isolation is inherent; this guards
// against a regression (e.g. a debug-assert abort returning).
func TestThrowingMicrotaskContained(t *testing.T) {
	eng, ctx := newTestEngine(t)
	sb := NewSandbox(eng, &testInvoker{})
	if _, err := sb.RunProgram(ctx,
		`try { queueMicrotask(function(){ throw new Error("boom"); }); } catch (e) {} return {done:true, answer:"a"};`); err != nil {
		t.Fatalf("run A: %v", err)
	}
	got, err := sb.RunProgram(ctx, `return {done:true, answer: 6*7};`)
	if err != nil {
		t.Fatalf("run B (reused instance corrupted?): %v", err)
	}
	if got != `{"done":true,"answer":42}` {
		t.Fatalf("run B got %q", got)
	}
}

// TestManyThrowingMicrotasksContained: MANY unhandled throwing microtasks in one
// program must not free the live guest context. rquickjs 0.9's execute_pending_job
// returns a JobException that would otherwise unbalance the context refcount, so
// ~500 throws would free the live context mid-drain (a use-after-free NDEBUG does not
// prevent). The guest forgets the exception; 2000 >> that threshold, and a reused
// instance afterward must still work.
func TestManyThrowingMicrotasksContained(t *testing.T) {
	eng, ctx := newTestEngine(t)
	sb := NewSandbox(eng, &testInvoker{})
	got, err := sb.RunProgram(ctx,
		`for (let i = 0; i < 2000; i++) queueMicrotask(function () { throw new Error("x"); }); return {done:true, answer: 6*7};`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != `{"done":true,"answer":42}` {
		t.Fatalf("got %q", got)
	}
	if got2, err := sb.RunProgram(ctx, `return 1 + 1;`); err != nil || got2 != "2" {
		t.Fatalf("reuse after many throwing microtasks: got %q err %v", got2, err)
	}
}

// TestPrettyJS: the log pretty-printer re-indents by bracket depth and is
// string-aware — brackets inside a string literal must not skew the indentation.
func TestPrettyJS(t *testing.T) {
	in := "const a = await x({k:\"}{\"});\nif (a) {\nreturn {done:true, answer: a};\n}"
	want := "const a = await x({k:\"}{\"});\nif (a) {\n  return {done:true, answer: a};\n}"
	if got := prettyJS(in); got != want {
		t.Fatalf("prettyJS mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestToolInvalidJSON: a tool returning non-JSON yields a clear, tool-named error.
func TestToolInvalidJSON(t *testing.T) {
	eng, ctx := newTestEngine(t)
	inv := &testInvoker{}
	inv.add("bad", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage("this is not json"), nil
	})
	_, err := NewSandbox(eng, inv).RunProgram(ctx, `await bad({}); return {done:true, answer:"x"};`)
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("expected tool-named invalid-JSON error, got %v", err)
	}
}

// TestFinalAnswerRequiresAnswer: {done:true} without a usable answer is NOT a
// completion (fed back), so the agent never silently returns an empty answer.
func TestFinalAnswerRequiresAnswer(t *testing.T) {
	cases := []struct {
		in       string
		wantAns  string
		wantDone bool
	}{
		{`{"done":true,"answer":"hi"}`, "hi", true},
		{`{"done":true,"answer":{"x":1}}`, `{"x":1}`, true},
		{`{"done":true}`, "", false},
		{`{"done":true,"answer":null}`, "", false},
		{`{"done":true,"answer":""}`, "", false},
		{`{"done":false,"answer":"hi"}`, "", false},
		{`{"foo":1}`, "", false},
	}
	for _, tc := range cases {
		ans, done := finalAnswer(tc.in)
		if ans != tc.wantAns || done != tc.wantDone {
			t.Fatalf("finalAnswer(%s) = (%q,%v), want (%q,%v)", tc.in, ans, done, tc.wantAns, tc.wantDone)
		}
	}
}

// TestWindowTurns: only the most-recent turns within the char budget are kept
// (always at least the last one).
func TestWindowTurns(t *testing.T) {
	turns := []Turn{{Content: "aaaa"}, {Content: "bbbb"}, {Content: "cccc"}} // 4 chars each
	if w := windowTurns(turns, 8); len(w) != 2 || w[0].Content != "bbbb" {
		t.Fatalf("budget 8: got %v", w)
	}
	if w := windowTurns(turns, 1000); len(w) != 3 {
		t.Fatalf("budget 1000: want all 3, got %d", len(w))
	}
	if w := windowTurns(turns, 1); len(w) != 1 || w[0].Content != "cccc" {
		t.Fatalf("budget 1: want just the last, got %v", w)
	}
}

// TestObservationClipped: a huge tool result is clipped before it becomes an
// observation, so the model context can't be overflowed by tool output.
func TestObservationClipped(t *testing.T) {
	eng, ctx := newTestEngine(t)
	inv := &testInvoker{}
	inv.add("blob", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(fmt.Sprintf(`{"data":%q}`, strings.Repeat("x", 50_000))), nil
	})
	model := &scriptModel{steps: []func(*Conversation) Decision{
		func(_ *Conversation) Decision {
			return Decision{Code: `const b = await blob({}); return {done:false, data:b.data};`}
		},
		func(_ *Conversation) Decision { return Decision{Code: `return {done:true, answer:"ok"};`} },
	}}
	convo := NewConversation("go")
	if _, err := RunAgent(ctx, NewSandbox(eng, inv), model, convo, 5, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	var obs string
	for _, tn := range convo.Turns {
		if tn.Role == RoleObservation {
			obs = tn.Content
		}
	}
	if len(obs) > maxObservationChars+100 {
		t.Fatalf("observation not clipped: %d chars", len(obs))
	}
	if !strings.Contains(obs, "truncated") {
		t.Fatalf("expected a truncation marker in the observation")
	}
}

func intToAnswer(v int) string {
	return "the answer is " + itoa(v)
}

func itoa(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// --- test-local doubles (a tiny calc tool + a session-aware scripted model) ---

// evalExpr evaluates "a*b" or "a+b".
func evalExpr(expr string) int {
	for _, op := range []byte{'*', '+'} {
		if i := strings.IndexByte(expr, op); i > 0 {
			a, _ := strconv.Atoi(strings.TrimSpace(expr[:i]))
			b, _ := strconv.Atoi(strings.TrimSpace(expr[i+1:]))
			if op == '*' {
				return a * b
			}
			return a + b
		}
	}
	n, _ := strconv.Atoi(strings.TrimSpace(expr))
	return n
}

// testCalc is a context-tool handler: {"expr":"6*7"} -> {"value":42}.
func testCalc(_ context.Context, arg json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Expr string `json:"expr"`
	}
	_ = json.Unmarshal(arg, &in)
	return json.RawMessage(fmt.Sprintf(`{"value":%d}`, evalExpr(in.Expr))), nil
}

// demoModel is a session-aware scripted planner: it computes with calc, and a
// follow-up "add N" builds on the prior answer found in the transcript.
type demoModel struct{}

func (demoModel) Decide(_ context.Context, convo *Conversation) (Decision, error) {
	last := convo.Turns[len(convo.Turns)-1]
	if last.Role == RoleObservation {
		var obs struct {
			Value int `json:"value"`
		}
		_ = json.Unmarshal([]byte(last.Content), &obs)
		return Decision{Code: fmt.Sprintf(`return {done:true, answer:"the answer is %d"};`, obs.Value)}, nil
	}
	if n, ok := parseAddN(last.Content); ok {
		return Decision{Code: fmt.Sprintf(`const c = await calc({expr:"%d+%d"}); return {done:false, value:c.value};`, lastAnswerNumber(convo), n)}, nil
	}
	return Decision{Code: `const c = await calc({expr:"6*7"}); return {done:false, value:c.value};`}, nil
}

func parseAddN(msg string) (int, bool) {
	f := strings.Fields(strings.ToLower(msg))
	for i, w := range f {
		if w == "add" && i+1 < len(f) {
			if n, err := strconv.Atoi(f[i+1]); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func lastAnswerNumber(convo *Conversation) int {
	for i := len(convo.Turns) - 1; i >= 0; i-- {
		if convo.Turns[i].Role == RoleAnswer {
			for _, f := range strings.Fields(convo.Turns[i].Content) {
				if n, err := strconv.Atoi(f); err == nil {
					return n
				}
			}
		}
	}
	return 0
}
