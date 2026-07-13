package agent

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// guest wraps a live guest instance with its memory and cached export handles. The
// guest is a LIVE coroutine (see engine.go): start(script) begins a program and
// returns the first step; resolve(handle,json)/reject(handle,msg) settle a pending
// promise and return the next step. State persists in the instance across these
// calls, so one instance runs one program at a time (exclusive checkout).
type guest struct {
	mod api.Module
	mem api.Memory

	fnAlloc   api.Function
	fnDealloc api.Function
	fnStart   api.Function
	fnResolve api.Function
	fnReject  api.Function

	runs int // programs this pooled instance has run; drives recycling
}

func newGuest(mod api.Module) *guest {
	return &guest{
		mod:       mod,
		mem:       mod.Memory(),
		fnAlloc:   mod.ExportedFunction("guest_alloc"),
		fnDealloc: mod.ExportedFunction("guest_dealloc"),
		fnStart:   mod.ExportedFunction("start"),
		fnResolve: mod.ExportedFunction("resolve"),
		fnReject:  mod.ExportedFunction("reject"),
	}
}

// call1 invokes a guest export returning a single scalar. A wasm trap panics here
// (as does a Restate cancellation surfaced by the driver); the panic propagates out
// of Engine.RunLive to the SDK handler rather than being threaded through every call
// site — RunLive no longer recovers it (see commit "one should not catch panics").
func call1(ctx context.Context, fn api.Function, args ...uint64) uint64 {
	res, err := fn.Call(ctx, args...)
	if err != nil {
		panic(fmt.Errorf("guest call %s: %w", fn.Definition().Name(), err))
	}
	if len(res) == 0 {
		return 0
	}
	return res[0]
}

func (g *guest) alloc(ctx context.Context, size uint32) uint32 {
	return uint32(call1(ctx, g.fnAlloc, uint64(size)))
}

func (g *guest) dealloc(ctx context.Context, ptr, size uint32) {
	if g.fnDealloc == nil || ptr == 0 || size == 0 {
		return
	}
	_, _ = g.fnDealloc.Call(ctx, uint64(ptr), uint64(size))
}

func (g *guest) write(ptr uint32, b []byte) {
	if len(b) == 0 {
		return
	}
	if !g.mem.Write(ptr, b) {
		panic("guest write out of range")
	}
}

// readBlob reads a step blob whose location the guest packed as (ptr<<32)|len, then
// frees the guest buffer. A zero-length blob is an error.
func (g *guest) readBlob(ctx context.Context, packed uint64) ([]byte, error) {
	outPtr := uint32(packed >> 32)
	outLen := uint32(packed & 0xFFFFFFFF)
	if outLen == 0 {
		return nil, fmt.Errorf("guest produced no output")
	}
	b, ok := g.mem.Read(outPtr, outLen)
	if !ok {
		g.dealloc(ctx, outPtr, outLen)
		return nil, fmt.Errorf("guest output out of range")
	}
	out := make([]byte, outLen) // copy before freeing the guest buffer
	copy(out, b)
	g.dealloc(ctx, outPtr, outLen)
	return out, nil
}

// start begins a program: copies the assembled script into guest memory, calls the
// `start` export, and returns the first step blob.
func (g *guest) start(ctx context.Context, script []byte) ([]byte, error) {
	ptr := g.alloc(ctx, uint32(len(script)))
	g.write(ptr, script)
	packed := call1(ctx, g.fnStart, uint64(ptr), uint64(len(script)))
	g.dealloc(ctx, ptr, uint32(len(script))) // the guest copied it
	return g.readBlob(ctx, packed)
}

// resolve settles pending promise `handle` with the JSON value bytes; reject settles
// it with an error message. Both return the next step blob.
func (g *guest) resolve(ctx context.Context, handle int, value []byte) ([]byte, error) {
	return g.settle(ctx, g.fnResolve, handle, value)
}

func (g *guest) reject(ctx context.Context, handle int, msg []byte) ([]byte, error) {
	return g.settle(ctx, g.fnReject, handle, msg)
}

func (g *guest) settle(ctx context.Context, fn api.Function, handle int, payload []byte) ([]byte, error) {
	ptr := g.alloc(ctx, uint32(len(payload)))
	g.write(ptr, payload)
	packed := call1(ctx, fn, uint64(uint32(int32(handle))), uint64(ptr), uint64(len(payload)))
	g.dealloc(ctx, ptr, uint32(len(payload)))
	return g.readBlob(ctx, packed)
}
