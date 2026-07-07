package agent

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// guest wraps a live guest instance with its memory and cached export handles. The
// guest is a stateless one-shot evaluator (see engine.go): the only real operation
// is execute(script) -> output blob.
type guest struct {
	mod api.Module
	mem api.Memory

	fnAlloc   api.Function
	fnDealloc api.Function
	fnExecute api.Function

	runs int // programs this pooled instance has run; drives recycling
}

func newGuest(mod api.Module) *guest {
	return &guest{
		mod:       mod,
		mem:       mod.Memory(),
		fnAlloc:   mod.ExportedFunction("guest_alloc"),
		fnDealloc: mod.ExportedFunction("guest_dealloc"),
		fnExecute: mod.ExportedFunction("execute"),
	}
}

// call1 invokes a guest export returning a single scalar. A wasm trap panics here
// and is recovered into an error in Engine.Run, rather than threading errors
// through every call site.
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

// execute runs one assembled script. It copies the script into guest memory, calls
// the `execute` export (which returns the output-blob location packed as
// (ptr<<32)|len), reads the blob out, and frees both buffers. A trap in the guest
// call panics via call1 and is recovered by Engine.Run.
func (g *guest) execute(ctx context.Context, script []byte) ([]byte, error) {
	scriptPtr := g.alloc(ctx, uint32(len(script)))
	g.write(scriptPtr, script)
	packed := call1(ctx, g.fnExecute, uint64(scriptPtr), uint64(len(script)))
	g.dealloc(ctx, scriptPtr, uint32(len(script))) // the guest copied it

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
