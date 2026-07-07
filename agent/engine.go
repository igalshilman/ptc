// engine.go drives the embedded QuickJS WASM guest (built from guest-rs/) via
// wazero, using a RE-EXECUTION (replay) model rather than a live, suspended program.
//
// The guest is a stateless one-shot evaluator: `execute(script)` runs an assembled
// script to synchronous quiescence and returns an output blob — it never calls back
// into the host and holds nothing between calls. A program is run as a *conversation*:
//
//	loop:
//	  script  = assemble(journal)                 // determinism + tool/journal prelude + program
//	  out     = guest.execute(script)             // run to first unresolved tool call
//	  done    -> return the program's answer
//	  error   -> return a program error (fed back to the model)
//	  frontier-> resolve those tool calls durably, append results to the journal, loop
//
// Each round re-runs the program from the top; already-resolved tool calls return
// their journaled results, so it advances to the next unresolved call. This mirrors
// Restate's own durable replay, one level down — and it means the guest keeps no
// per-program state, so there are no cross-call globals. Guest instances are still
// POOLED (acquire/release) purely to avoid re-instantiation churn; reuse is trivially
// safe because the guest is stateless.
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

// Guest output status — must match guest-rs (the "s" field of the output blob).
const (
	outDone  = 0 // program settled: "r" holds its return value
	outCalls = 1 // program blocked: "frontier" holds the new tool calls to run
	outError = 2 // program error (throw / deadlock): "error" holds the message
)

// guestOutput is the JSON blob execute() returns.
type guestOutput struct {
	S        int             `json:"s"`
	R        json.RawMessage `json:"r"`
	Frontier []frontierCall  `json:"frontier"`
	Error    string          `json:"error"`
}

type frontierCall struct {
	Name string          `json:"name"`
	Arg  json.RawMessage `json:"arg"`
}

// maxProgramRounds bounds the re-execution loop (a program making this many
// sequential tool calls is pathological); the per-program timeout is the real guard.
const maxProgramRounds = 4096

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

// Run drives one program via the re-execution loop. `assemble(journal)` builds the
// script for a round (injecting the journal of results so far); `resolve` runs a
// frontier of tool calls durably and returns their results (to append to the
// journal). It returns the program's answer (JSON), or a program error to feed back
// to the model. A guest trap / timeout / Restate cancellation is recovered here.
func (e *Engine) Run(
	ctx context.Context,
	assemble func(journal []ToolResult) string,
	resolve func(ctx context.Context, calls []ToolCall) []ToolResult,
) (result string, err error) {
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
		if r := recover(); r != nil {
			result = ""
			switch v := r.(type) {
			case fatalError:
				err = v.err
			default:
				if ce := ctx.Err(); ce != nil {
					err = fmt.Errorf("program exceeded its time limit: %w", ce)
				} else {
					err = fmt.Errorf("guest execution failed: %v", r)
				}
			}
		}
		// Reuse the instance unless a trap/timeout fired (recovered above, so healthy
		// is still false). The guest is stateless, so a mere program error is fine.
		e.release(ctx, g, healthy && ctx.Err() == nil)
	}()

	var journal []ToolResult
	for range maxProgramRounds {
		out, execErr := g.execute(ctx, []byte(assemble(journal)))
		if execErr != nil {
			return "", execErr
		}
		var o guestOutput
		if e2 := json.Unmarshal(out, &o); e2 != nil {
			healthy = true
			return "", fmt.Errorf("guest returned invalid output: %v", e2)
		}
		switch o.S {
		case outDone:
			healthy = true
			return string(o.R), nil
		case outError:
			healthy = true
			return "", fmt.Errorf("js error: %s", o.Error)
		case outCalls:
			frontier := make([]ToolCall, len(o.Frontier))
			for i, c := range o.Frontier {
				frontier[i] = ToolCall{Tool: c.Name, Arg: c.Arg}
			}
			journal = append(journal, resolve(ctx, frontier)...)
		default:
			healthy = true
			return "", fmt.Errorf("guest returned unknown status %d", o.S)
		}
	}
	healthy = true
	return "", fmt.Errorf("program exceeded %d execution rounds", maxProgramRounds)
}
