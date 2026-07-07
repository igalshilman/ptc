// engine.go drives the embedded QuickJS WASM guest (built from guest-rs/) via
// wazero.
//
// The guest is Restate-agnostic: it exposes an async-JS executor over a small ABI
// (eval_code / run_microtasks / resolve_handle / get_pending_* / get_result_* /
// guest_alloc) and imports ONE generic host function — env.host_call(name, arg),
// returning an integer handle. The host owns the event loop: evaluate -> collect
// pending host calls -> resolve them -> drain microtasks -> repeat.
//
// The Resolver maps each pending call onto a durable Restate operation (see
// restateInvoker in service.go); guest instances are pooled and reused across runs
// (acquire/release), reset by each eval_code.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// The WASI clock/rand are pinned to these FIXED constants on every instance (see
// Engine.Run). They must never vary — a value that changed per run could not be
// applied to a REUSED (pooled) instance, whose WASI config is bound at
// instantiation. The per-invocation seed/clock a program actually observes is
// injected in JS by Sandbox.determinismPrelude, which is re-run every program; this
// pin is only defense-in-depth against a nondeterminism source the prelude missed.
const (
	detFixedEpochSec int64  = 1_700_000_000 // arbitrary fixed wall-clock second
	detFixedSeed     uint64 = 0x9e3779b97f4a7c15
)

// lcgReader is a deterministic byte source (fixed seed) used as the guest's WASI
// random source, so any WASI getrandom the engine seeds from is replay-stable.
type lcgReader struct{ state uint64 }

func (r *lcgReader) Read(p []byte) (int, error) {
	// Non-mutating: derive bytes into a local so r.state never changes. That keeps
	// the WASI rand replay- AND reuse-stable (a pooled instance never diverges from
	// a fresh one) with no shared-state race. It's only a defense-in-depth backstop —
	// JS Math.random/crypto are overridden per run in the prelude.
	s := r.state
	for i := range p {
		s = s*6364136223846793005 + 1442695040888963407
		p[i] = byte(s >> 56)
	}
	return len(p), nil
}

// Guest status codes — must match guest-rs/src/lib.rs.
const (
	statusDone       int32 = 0
	statusHasPending int32 = 1
	statusError      int32 = 2
)

// hostErrHandle is returned by a host import when there is no active run.
const hostErrHandle uint32 = 0xFFFFFFFF // -1 as i32

// HostCall is one async operation the JS guest is waiting on: the guest called
// __hostCall(Tool, Arg) and is awaiting the promise for Handle.
type HostCall struct {
	Handle uint32
	Tool   string
	Arg    string // the argument the JS caller passed (JSON string)
}

// HostResult resolves a HostCall back into the guest promise.
type HostResult struct {
	Handle  uint32
	Value   string
	IsError bool
}

// Resolver turns a batch of pending host calls into results. In production this is
// restateInvoker (service.go), which runs each call as a durable Restate step; the
// tests use in-memory doubles.
type Resolver interface {
	Resolve(ctx context.Context, calls []HostCall) []HostResult
}

// fatalError signals that program execution must ABORT with this error rather than
// be fed back to the model as a recoverable observation. A Resolver panics with it
// for invocation-fatal conditions (e.g. a Restate cancellation reported by the
// batch driver); Engine.Run's recover surfaces err verbatim so its terminal-ness
// is preserved all the way up to the Ask handler (which then does not persist
// state). It unwinds only pure Go stack — the wasm call has already returned before
// Resolve runs — so it is safe.
type fatalError struct{ err error }

// runState is per-invocation state carried through the wazero call context, so
// the shared `env` host functions can record calls against the active run.
type runState struct {
	next    uint32
	pending map[uint32]*HostCall
}

func (s *runState) record(tool, arg string) uint32 {
	s.next++
	h := s.next
	s.pending[h] = &HostCall{Handle: h, Tool: tool, Arg: arg}
	return h
}

type ctxStateKey struct{}

func stateFrom(ctx context.Context) *runState {
	st, _ := ctx.Value(ctxStateKey{}).(*runState)
	return st
}

// Engine holds the one-time wazero setup: a runtime with WASI + the `env` host
// module registered, and the guest compiled once. Guest instances are POOLED and
// reused across Runs (see acquire/release): an instance is checked out EXCLUSIVELY
// per Run and reset by the next eval_code (fresh JSContext + cleared pending/result
// in the Rust guest), which avoids the ~10 MB/Run re-instantiation churn while
// keeping per-run isolation. Exclusive checkout is what makes reuse safe for the
// single-threaded guest — never hand one instance to two goroutines at once.
type Engine struct {
	runtime       wazero.Runtime
	compiled      wazero.CompiledModule
	hasInitialize bool        // guest is a WASI reactor (exports _initialize) vs a plain cdylib
	free          chan *guest // idle, reset-ready instances available for reuse

	mu       sync.Mutex     // guards closing; serializes it against inflight.Add
	closing  bool           // set by Close; rejects new Runs
	inflight sync.WaitGroup // in-flight Runs, so Close can drain before closing the runtime
}

// errEngineClosed is returned by Run once Close has begun.
var errEngineClosed = errors.New("engine is closing")

// enter registers an in-flight Run, or reports that the engine is closing. The
// mutex serializes the closing check + Add against Close setting closing, so no
// Add can race with (or follow) inflight.Wait.
func (e *Engine) enter() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return false
	}
	e.inflight.Add(1)
	return true
}

// maxMemoryPages caps each guest instance's linear memory (pages × 64 KiB), a
// host-side backstop against a runaway program allocating until OOM. 4096 pages
// = 256 MiB.
const maxMemoryPages = 4096

// Pool bounds: how many idle instances to keep, and when to retire a reused one so
// a long-lived JSRuntime's interned atoms / grown linear memory can't accumulate
// forever.
const (
	poolMaxIdle          = 32       // idle instances kept for reuse (excess are closed)
	maxRunsPerInstance   = 256      // recycle an instance after this many programs
	instanceMemHighWater = 64 << 20 // recycle an instance whose linear memory grew past this
)

// NewEngine builds the runtime, registers WASI preview1 and the `env` host
// module, and compiles the guest. The runtime is configured so a per-program
// context deadline can interrupt a runaway program (WithCloseOnContextDone) and
// so no instance can exceed maxMemoryPages.
func NewEngine(ctx context.Context, wasm []byte) (*Engine, error) {
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(maxMemoryPages))

	// The guest is a wasm32-wasi reactor and imports fd_write/clock_time_get/…
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	// Host module `env` — a single generic import. The guest calls
	// host_call(name, arg); we read both strings from guest memory, record a
	// pending call on the ctx runState, and return the handle the guest turns
	// into a Promise. The guest hardcodes no tool names.
	if _, err := r.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(hostCall).Export("host_call").
		Instantiate(ctx); err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("register env host module: %w", err)
	}

	compiled, err := r.CompileModule(ctx, wasm)
	if err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("compile guest: %w", err)
	}
	// A reactor guest exports `_initialize` (must run before other exports); our
	// Rust cdylib guest has none (Rust inits statics lazily), so this is false —
	// the check keeps the driver correct if a reactor guest is ever swapped in.
	_, hasInit := compiled.ExportedFunctions()["_initialize"]
	return &Engine{
		runtime:       r,
		compiled:      compiled,
		hasInitialize: hasInit,
		free:          make(chan *guest, poolMaxIdle),
	}, nil
}

// Close stops accepting new Runs, waits (bounded by ctx) for in-flight Runs to
// finish so runtime.Close can't pull an instance out from under an active guest
// call, then drains and closes any pooled instances and releases the runtime. If
// ctx fires first, it closes anyway (any still-running Run then traps, recovered
// into an error) — best-effort graceful shutdown.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	e.closing = true
	e.mu.Unlock()

	drained := make(chan struct{})
	go func() { e.inflight.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-ctx.Done():
	}

	for {
		select {
		case g := <-e.free:
			_ = g.mod.Close(ctx)
		default:
			return e.runtime.Close(ctx)
		}
	}
}

// newModuleConfig builds a guest instance's config. It is FIXED (no per-run values)
// so a pooled instance never needs re-configuration: the WASI clock/rand are pinned
// to constants (per-run determinism lives in the JS prelude), stdio goes to the
// host, and the start function depends on the guest kind. A fresh lcgReader per
// instance keeps the rand source unshared (no cross-instance data race).
func (e *Engine) newModuleConfig() wazero.ModuleConfig {
	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous: allow many concurrent instances
		WithStdout(os.Stdout).WithStderr(os.Stderr).
		WithWalltime(func() (int64, int32) { return detFixedEpochSec, 0 }, sys.ClockResolution(1_000_000)).
		WithNanotime(func() int64 { return 0 }, sys.ClockResolution(1_000_000)).
		WithRandSource(&lcgReader{state: detFixedSeed})
	if e.hasInitialize {
		return cfg.WithStartFunctions("_initialize") // reactor init — NOT the default _start
	}
	return cfg.WithStartFunctions() // cdylib (Rust): no start function
}

// acquire checks out an idle instance from the pool, or instantiates a fresh one.
// New instances are created with a background context so their lifetime is NOT tied
// to any Run's (possibly-cancelled/timed-out) context — only the guest CALLS carry
// the per-Run context (for the timeout + runState).
func (e *Engine) acquire() (*guest, error) {
	select {
	case g := <-e.free:
		return g, nil
	default:
		mod, err := e.runtime.InstantiateModule(context.Background(), e.compiled, e.newModuleConfig())
		if err != nil {
			return nil, fmt.Errorf("instantiate guest: %w", err)
		}
		return newGuest(mod), nil
	}
}

// release returns a healthy instance to the pool for reuse, or closes it. An
// instance is retired (not reused) if the Run failed/timed out (it may be a
// wazero-closed or inconsistent instance), if it has served enough runs, or if its
// linear memory grew past the high-water mark (it never shrinks).
func (e *Engine) release(ctx context.Context, g *guest, healthy bool) {
	if !healthy ||
		g.runs >= maxRunsPerInstance ||
		(g.mem != nil && uint64(g.mem.Size()) > instanceMemHighWater) {
		_ = g.mod.Close(ctx)
		return
	}
	select {
	case e.free <- g:
	default:
		_ = g.mod.Close(ctx) // pool full
	}
}

// hostCall implements env.host_call(name_ptr, name_len, arg_ptr, arg_len) -> handle.
// It reads the tool name and argument from guest memory, records the pending call,
// and returns the handle (or -1 if there is no active run).
func hostCall(ctx context.Context, m api.Module, namePtr, nameLen, argPtr, argLen uint32) uint32 {
	st := stateFrom(ctx)
	if st == nil {
		return hostErrHandle
	}
	return st.record(readString(m, namePtr, nameLen), readString(m, argPtr, argLen))
}

// Run evaluates JS `code` to completion, resolving each async host call through
// resolver, and returns the JS return value (as produced by the guest's
// JS_ToCString — callers should JSON.stringify structured values).
//
// Guest calls trap as panics (see guest.go/call1); Run recovers them into an
// error. If the call context's deadline fired (a runaway program hitting its
// time budget) or the memory limit was hit, that surfaces here as a normal error
// the caller can feed back to the model — not a crashed goroutine.
func (e *Engine) Run(ctx context.Context, code string, resolver Resolver) (result string, err error) {
	if !e.enter() {
		return "", errEngineClosed
	}
	defer e.inflight.Done() // registered first ⇒ runs LAST, after release below

	st := &runState{pending: map[uint32]*HostCall{}}
	ctx = context.WithValue(ctx, ctxStateKey{}, st)

	g, err := e.acquire() // reused from the pool, or freshly instantiated
	if err != nil {
		return "", err
	}
	g.runs++
	healthy := false
	defer func() {
		if r := recover(); r != nil {
			result = ""
			switch v := r.(type) {
			case fatalError:
				// Invocation-fatal (e.g. a Restate cancellation): keep err verbatim so
				// its terminal-ness reaches RunAgent/Ask instead of being swallowed.
				err = v.err
			default:
				if ce := ctx.Err(); ce != nil {
					err = fmt.Errorf("program exceeded its time limit: %w", ce)
				} else {
					err = fmt.Errorf("guest execution failed: %v", r)
				}
			}
		}
		// Reuse the instance only if the program reached a clean guest status and the
		// context didn't fire; a trap/timeout (recovered above, so healthy is still
		// false) may have left it wazero-closed, so it is retired instead. A mere
		// program error (js error / rejection) keeps healthy=true — the next
		// eval_code resets it with a fresh context, so it is safe to reuse.
		e.release(ctx, g, healthy && ctx.Err() == nil)
	}()

	codePtr := g.alloc(ctx, uint32(len(code)))
	g.write(codePtr, []byte(code))
	status := g.evalCode(ctx, codePtr, uint32(len(code)))
	g.dealloc(ctx, codePtr, uint32(len(code))) // the guest copied the code

	for {
		switch status {
		case statusDone:
			healthy = true // program-level result or rejection; instance is reusable
			return g.result(ctx)

		case statusError:
			msg := g.errorResult(ctx)
			if msg == "" {
				msg = "no pending ops and promise not settled (deadlock)"
			}
			healthy = true // a program error, not a trap; a reset makes it reusable
			return "", fmt.Errorf("js error: %s", msg)

		case statusHasPending:
			handles := g.pendingHandles(ctx)
			calls := make([]HostCall, 0, len(handles))
			for _, h := range handles {
				if c := st.pending[h]; c != nil {
					calls = append(calls, *c)
				} else {
					calls = append(calls, HostCall{Handle: h, Tool: "unknown"})
				}
			}

			for _, res := range resolver.Resolve(ctx, calls) {
				valPtr := g.alloc(ctx, uint32(len(res.Value)))
				g.write(valPtr, []byte(res.Value))
				isErr := uint32(0)
				if res.IsError {
					isErr = 1
				}
				g.resolveHandle(ctx, res.Handle, valPtr, uint32(len(res.Value)), isErr)
				g.dealloc(ctx, valPtr, uint32(len(res.Value))) // the guest copied the value
				delete(st.pending, res.Handle)
			}
			status = g.runMicrotasks(ctx)

		default:
			return "", fmt.Errorf("unexpected guest status %d", status)
		}
	}
}

// readString reads a UTF-8 string out of a module's linear memory. The returned
// []byte from Memory().Read aliases wasm memory, but string() copies it, so the
// result is safe to keep.
func readString(m api.Module, ptr, ln uint32) string {
	if ln == 0 {
		return ""
	}
	b, ok := m.Memory().Read(ptr, ln)
	if !ok {
		return ""
	}
	return string(b)
}
