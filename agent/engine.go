// Package main embeds the QuickJS WASM guest (quickjs-guest/guest.c) and drives
// it from Go using wazero.
//
// The guest is Restate-agnostic: it exposes an async-JS executor over a small
// ABI (eval_code / run_microtasks / resolve_handle / get_pending_handles /
// get_result_*) and imports host functions (webSearch/bash/catalog/summaries/
// sleep) that each return an integer handle. The host owns the event loop:
// evaluate -> collect pending host calls -> resolve them -> drain microtasks ->
// repeat, exactly like the Rust quickjs-worker but on wazero instead of wasmtime.
//
// In this spike the pending calls are resolved by a mock Resolver. In the real
// worker the Resolver will map each call onto a durable Restate operation
// (restate.Run for tool/OpenAI calls, restate.Sleep for sleep) so results are
// journaled and replayed.
package agent

import (
	"context"
	"fmt"
	"os"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// determinism, when present on the call context, freezes the guest's WASI clock
// and randomness so a program re-run on replay produces identical results (e.g.
// `new Date()` and any WASI getrandom). The Math.random/Date.now JS builtins are
// additionally overridden in the sandbox prelude. Sandbox.RunProgram sets this;
// direct engine callers (tests) omit it and get the real clock/rand.
type determinism struct {
	nowMillis int64
	randSeed  int64
}

type ctxDetKey struct{}

// lcgReader is a deterministic byte source seeded from a replay-stable seed, used
// as the guest's WASI random source under determinism.
type lcgReader struct{ state uint64 }

func (r *lcgReader) Read(p []byte) (int, error) {
	for i := range p {
		r.state = r.state*6364136223846793005 + 1442695040888963407
		p[i] = byte(r.state >> 56)
	}
	return len(p), nil
}

// Guest status codes — must match quickjs-guest/guest.c.
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

// Resolver turns a batch of pending host calls into results. The spike uses a
// mock; the durable worker will call restate.Run / restate.Sleep here.
type Resolver interface {
	Resolve(ctx context.Context, calls []HostCall) []HostResult
}

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
// module registered, and the guest compiled once. Instantiate a fresh instance
// per Run for isolation.
type Engine struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
}

// maxMemoryPages caps each guest instance's linear memory (pages × 64 KiB), a
// host-side backstop against a runaway program allocating until OOM. 4096 pages
// = 256 MiB. (The guest's own QuickJS heap is uncapped — JS_SetMemoryLimit would
// need a guest.c rebuild — so this wazero limit is the enforced ceiling.)
const maxMemoryPages = 4096

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
	return &Engine{runtime: r, compiled: compiled}, nil
}

// Close releases the runtime.
func (e *Engine) Close(ctx context.Context) error { return e.runtime.Close(ctx) }

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
	defer func() {
		if r := recover(); r != nil {
			result = ""
			if ce := ctx.Err(); ce != nil {
				err = fmt.Errorf("program exceeded its time limit: %w", ce)
			} else {
				err = fmt.Errorf("guest execution failed: %v", r)
			}
		}
	}()

	st := &runState{pending: map[uint32]*HostCall{}}
	ctx = context.WithValue(ctx, ctxStateKey{}, st)

	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous: allow many concurrent instances
		WithStdout(os.Stdout).WithStderr(os.Stderr).
		WithStartFunctions("_initialize") // reactor init — NOT the default _start

	// Under determinism, freeze the WASI wall/monotonic clock and randomness so a
	// replayed program reproduces identical results (covers `new Date()` and any
	// WASI getrandom the engine seeds from).
	if det, ok := ctx.Value(ctxDetKey{}).(determinism); ok {
		sec, nsec := det.nowMillis/1000, int32((det.nowMillis%1000)*1_000_000)
		cfg = cfg.
			WithWalltime(func() (int64, int32) { return sec, nsec }, sys.ClockResolution(1_000_000)).
			WithNanotime(func() int64 { return det.nowMillis * 1_000_000 }, sys.ClockResolution(1_000_000)).
			WithRandSource(&lcgReader{state: uint64(det.randSeed) + 0x9e3779b97f4a7c15})
	}

	mod, err := e.runtime.InstantiateModule(ctx, e.compiled, cfg)
	if err != nil {
		return "", fmt.Errorf("instantiate guest: %w", err)
	}
	defer mod.Close(ctx)

	g := newGuest(mod)

	codePtr := g.alloc(ctx, uint32(len(code)))
	g.write(codePtr, []byte(code))
	status := g.evalCode(ctx, codePtr, uint32(len(code)))

	for {
		switch status {
		case statusDone:
			return g.result(ctx)

		case statusError:
			msg := g.errorResult(ctx)
			if msg == "" {
				msg = "no pending ops and promise not settled (deadlock)"
			}
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
