// engine.go drives the embedded QuickJS WASM guest (built from guest-rs/) via
// wazero, using a LIVE COROUTINE model: the guest runs the program ONCE and the host
// drives it to completion by settling its promises as durable operations finish.
//
//	out = guest.start(script)                    // determinism + bridge + program
//	loop:
//	  done  -> return the program's answer
//	  error -> return a program error (fed back to the model)
//	  ops   -> inv.Start(new ops)                // submit each as a durable future
//	           res = inv.Next()                  // race them; FIRST completion wins
//	           out = guest.resolve/reject(res)   // settle that one promise, get next step
//
// Settling promises one-at-a-time in completion order is what makes first-completion
// (Promise.race / timeouts) work, and it mirrors Restate's own Select: on crash the
// host re-runs `start` and feeds the journaled completions back in the same order, so
// the program re-derives identically. State lives in the guest for the duration of one
// program; the host owns durability. Instances are POOLED and checked out EXCLUSIVELY
// (one live program per instance at a time); a fresh `start` resets the guest state.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// The WASI clock/rand are pinned to these FIXED constants on every instance. They
// must never vary (a pooled instance's WASI config is bound at instantiation). The
// per-invocation seed/clock a program observes is injected in JS by
// Sandbox.determinismPrelude; this pin is only defense-in-depth.
const (
	detFixedEpochSec int64  = 1_700_000_000
	detFixedSeed     uint64 = 0x9e3779b97f4a7c15
)

// lcgReader is a deterministic byte source (fixed seed) for the guest's WASI random
// source, so any WASI getrandom is replay-stable.
type lcgReader struct{ state uint64 }

func (r *lcgReader) Read(p []byte) (int, error) {
	// Non-mutating: derive into a local so r.state never changes — replay- and
	// reuse-stable, no shared-state race. Only a backstop (JS Math.random/crypto are
	// overridden per run in the prelude).
	s := r.state
	for i := range p {
		s = s*6364136223846793005 + 1442695040888963407
		p[i] = byte(s >> 56)
	}
	return len(p), nil
}

// Guest step status — must match guest-rs (the "s" field of the step blob).
const (
	outDone  = 0 // program settled: "r" holds its return value
	outCalls = 1 // program running: "ops" holds the new operations it started this step
	outError = 2 // program error (throw): "error" holds the message
)

// guestStep is the JSON blob each guest export (start/resolve/reject) returns.
type guestStep struct {
	S     int             `json:"s"`
	R     json.RawMessage `json:"r"`
	Ops   []guestOp       `json:"ops"`
	Error string          `json:"error"`
}

// guestOp is one operation the program started this step: a stable, deterministic
// handle plus the tool name and argument. The host settles the promise for `handle`
// once the corresponding durable future completes.
type guestOp struct {
	Handle int             `json:"handle"`
	Name   string          `json:"name"`
	Arg    json.RawMessage `json:"arg"`
}

// maxProgramSteps bounds the drive loop. In the live model each step settles ONE
// operation (one WaitFirst completion), so this caps the TOTAL operations a single
// program may complete — parallel width and sequential depth both count against it.
// It is the sole backstop against a runaway tool-calling loop now that there is no
// per-program timeout, so it is set well above any realistic fan-out. Nothing bounds
// wall-clock time inside a guest call; WithCloseOnContextDone only lets an invocation
// cancellation interrupt a stuck guest.
const maxProgramSteps = 65536

// fatalError signals that execution must ABORT with this error rather than be fed
// back to the model. The resolve callback panics with it on invocation-fatal
// conditions (e.g. a Restate cancellation); Engine.Run's recover surfaces err
// verbatim so its terminal-ness reaches the Ask handler.
type fatalError struct{ err error }

// Engine holds the one-time wazero setup (runtime + compiled guest) and a pool of
// reusable instances. The guest is stateless, so a pooled instance needs no reset
// and reuse is safe as long as one instance is used by one goroutine at a time
// (exclusive checkout via the free channel).
type Engine struct {
	runtime       wazero.Runtime
	compiled      wazero.CompiledModule
	hasInitialize bool
	free          chan *guest

	mu       sync.Mutex
	closing  bool
	inflight sync.WaitGroup
}

var errEngineClosed = errors.New("engine is closing")

func (e *Engine) enter() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return false
	}
	e.inflight.Add(1)
	return true
}

const maxMemoryPages = 4096 // 256 MiB linear-memory cap per instance

const (
	poolMaxIdle          = 32
	instanceMemHighWater = 64 << 20
	maxRunsPerInstance   = 256
)

// NewEngine builds the runtime (WASI only — the guest has no host imports) and
// compiles the guest.
func NewEngine(ctx context.Context, wasm []byte) (*Engine, error) {
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(maxMemoryPages))

	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	compiled, err := r.CompileModule(ctx, wasm)
	if err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("compile guest: %w", err)
	}
	_, hasInit := compiled.ExportedFunctions()["_initialize"]
	return &Engine{
		runtime:       r,
		compiled:      compiled,
		hasInitialize: hasInit,
		free:          make(chan *guest, poolMaxIdle),
	}, nil
}

// Close stops accepting new Runs, waits (bounded by ctx) for in-flight Runs so
// runtime.Close can't close an instance under an active guest call, then drains the
// pool and releases the runtime.
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

// newModuleConfig is FIXED (no per-run values), so a pooled instance never needs
// reconfiguration. A fresh lcgReader per instance keeps the rand source unshared.
func (e *Engine) newModuleConfig() wazero.ModuleConfig {
	cfg := wazero.NewModuleConfig().
		WithName("").
		WithStdout(os.Stdout).WithStderr(os.Stderr).
		WithWalltime(func() (int64, int32) { return detFixedEpochSec, 0 }, sys.ClockResolution(1_000_000)).
		WithNanotime(func() int64 { return 0 }, sys.ClockResolution(1_000_000)).
		WithRandSource(&lcgReader{state: detFixedSeed})
	if e.hasInitialize {
		return cfg.WithStartFunctions("_initialize")
	}
	return cfg.WithStartFunctions()
}

// acquire checks out an idle instance or instantiates a fresh one (with a background
// context, so its lifetime isn't tied to any Run's context).
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

// release returns a healthy instance to the pool, or closes it (on trap/timeout, or
// after enough runs / too much grown memory).
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
		_ = g.mod.Close(ctx)
	}
}

// RunLive drives one program as a live coroutine: `start` the assembled script, then
// repeatedly submit the ops it emits (inv.Start), race them for the first completion
// (inv.Next), and settle that promise in the guest (resolve/reject) to get the next
// step. It returns the program's answer (JSON), or a program error to feed back to
// the model. A guest trap or Restate cancellation is recovered here.
func (e *Engine) RunLive(ctx context.Context, script string, inv Invoker) (result string, err error) {
	if !e.enter() {
		return "", errEngineClosed
	}
	defer e.inflight.Done() // registered first ⇒ runs LAST, after release

	g, err := e.acquire()
	if err != nil {
		return "", err
	}
	g.runs++
	healthy := false
	defer func() {
		// if r := recover(); r != nil {
		// 	result = ""
		// 	switch v := r.(type) {
		// 	case fatalError:
		// 		err = v.err
		// 	default:
		// 		if ce := ctx.Err(); ce != nil {
		// 			err = fmt.Errorf("program interrupted: %w", ce)
		// 		} else {
		// 			err = fmt.Errorf("guest execution failed: %v", r)
		// 		}
		// 	}
		// }
		// Reuse the instance unless a trap fired (recovered above, so healthy is still
		// false). A fresh `start` resets the guest state, so a mere program error is fine.
		e.release(ctx, g, healthy && ctx.Err() == nil)
	}()

	inv.Reset() // clear any prior program's leftover ops; the guest resets handles per start
	out, err := g.start(ctx, []byte(script))
	if err != nil {
		return "", err
	}
	for range maxProgramSteps {
		var step guestStep
		if e2 := json.Unmarshal(out, &step); e2 != nil {
			healthy = true
			return "", fmt.Errorf("guest returned invalid output: %v", e2)
		}
		switch step.S {
		case outDone:
			healthy = true
			return string(step.R), nil
		case outError:
			healthy = true
			return "", fmt.Errorf("js error: %s", step.Error)
		case outCalls:
			if len(step.Ops) > 0 {
				calls := make([]ToolCall, len(step.Ops))
				for i, o := range step.Ops {
					calls[i] = ToolCall{Handle: o.Handle, Tool: o.Name, Arg: o.Arg}
				}
				inv.Start(calls)
			}
			if inv.Pending() == 0 {
				// No new ops and nothing in flight, yet the program hasn't settled — it
				// awaited something that can never complete (a JS-level deadlock).
				healthy = true
				return "", fmt.Errorf("program made no progress (awaiting with no pending operations)")
			}
			res, fatal := inv.Next(ctx)
			if fatal != nil {
				panic(fatalError{fatal})
			}
			if res.IsErr {
				out, err = g.reject(ctx, res.Handle, []byte(res.ErrMsg))
			} else {
				v := res.Value
				if len(v) == 0 {
					v = []byte("null")
				}
				out, err = g.resolve(ctx, res.Handle, v)
			}
			if err != nil {
				return "", err
			}
		default:
			healthy = true
			return "", fmt.Errorf("guest returned unknown status %d", step.S)
		}
	}
	healthy = true
	return "", fmt.Errorf("program exceeded %d steps", maxProgramSteps)
}
