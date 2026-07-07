package agent

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// guest wraps a live guest instance with its memory and cached export handles.
// Exports are looked up once at instantiate time, never in the hot loop.
type guest struct {
	mod api.Module
	mem api.Memory

	fnAlloc          api.Function
	fnDealloc        api.Function
	fnEval           api.Function
	fnMicrotasks     api.Function
	fnPendingCount   api.Function
	fnPendingHandles api.Function
	fnResolve        api.Function
	fnResultPtr      api.Function
	fnResultLen      api.Function
	fnResultIsError  api.Function
	fnErrorPtr       api.Function
	fnErrorLen       api.Function

	runs int // how many programs this (pooled) instance has run; drives recycling
}

func newGuest(mod api.Module) *guest {
	return &guest{
		mod:              mod,
		mem:              mod.Memory(),
		fnAlloc:          mod.ExportedFunction("guest_alloc"),
		fnDealloc:        mod.ExportedFunction("guest_dealloc"),
		fnEval:           mod.ExportedFunction("eval_code"),
		fnMicrotasks:     mod.ExportedFunction("run_microtasks"),
		fnPendingCount:   mod.ExportedFunction("get_pending_count"),
		fnPendingHandles: mod.ExportedFunction("get_pending_handles"),
		fnResolve:        mod.ExportedFunction("resolve_handle"),
		fnResultPtr:      mod.ExportedFunction("get_result_ptr"),
		fnResultLen:      mod.ExportedFunction("get_result_len"),
		fnResultIsError:  mod.ExportedFunction("get_result_is_error"),
		fnErrorPtr:       mod.ExportedFunction("get_error_ptr"),
		fnErrorLen:       mod.ExportedFunction("get_error_len"),
	}
}

// call1 invokes a guest export returning a single scalar. Wasm traps are fatal
// in this spike, so we surface them loudly rather than threading errors through
// every call site.
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

// dealloc frees a buffer obtained from alloc. It matters for REUSED (pooled)
// instances: without it, per-run host buffers (code/values/pending handles) would
// accumulate in the instance's linear memory across runs. size must match alloc.
func (g *guest) dealloc(ctx context.Context, ptr, size uint32) {
	if g.fnDealloc == nil || ptr == 0 || size == 0 {
		return
	}
	_, _ = g.fnDealloc.Call(ctx, uint64(ptr), uint64(size))
}

func (g *guest) evalCode(ctx context.Context, ptr, ln uint32) int32 {
	return int32(uint32(call1(ctx, g.fnEval, uint64(ptr), uint64(ln))))
}

func (g *guest) runMicrotasks(ctx context.Context) int32 {
	return int32(uint32(call1(ctx, g.fnMicrotasks)))
}

func (g *guest) resolveHandle(ctx context.Context, handle, valPtr, valLen, isErr uint32) {
	if _, err := g.fnResolve.Call(ctx, uint64(handle), uint64(valPtr), uint64(valLen), uint64(isErr)); err != nil {
		panic(fmt.Errorf("resolve_handle: %w", err))
	}
}

func (g *guest) pendingHandles(ctx context.Context) []uint32 {
	count := uint32(call1(ctx, g.fnPendingCount))
	if count == 0 {
		return nil
	}
	out := g.alloc(ctx, count*4)
	n := uint32(call1(ctx, g.fnPendingHandles, uint64(out), uint64(count)))
	handles := make([]uint32, 0, n)
	for i := uint32(0); i < n; i++ {
		h, ok := g.mem.ReadUint32Le(out + i*4)
		if !ok {
			panic("get_pending_handles: read out of range")
		}
		handles = append(handles, h)
	}
	g.dealloc(ctx, out, count*4) // handles copied out; free the guest buffer
	return handles
}

// result reads the settled main-promise value. If the guest flagged an error it
// reads the error buffer instead (the guest writes rejections/compile errors to
// a separate buffer — a bug the Rust worker had by reading the result buffer in
// the error path; here we read the right one).
func (g *guest) result(ctx context.Context) (string, error) {
	if uint32(call1(ctx, g.fnResultIsError)) != 0 {
		return "", fmt.Errorf("js rejected: %s", g.errorResult(ctx))
	}
	ptr := uint32(call1(ctx, g.fnResultPtr))
	ln := uint32(call1(ctx, g.fnResultLen))
	return g.readStr(ptr, ln), nil
}

func (g *guest) errorResult(ctx context.Context) string {
	ptr := uint32(call1(ctx, g.fnErrorPtr))
	ln := uint32(call1(ctx, g.fnErrorLen))
	return g.readStr(ptr, ln)
}

func (g *guest) write(ptr uint32, b []byte) {
	if len(b) == 0 {
		return
	}
	if !g.mem.Write(ptr, b) {
		panic("guest write out of range")
	}
}

func (g *guest) readStr(ptr, ln uint32) string {
	if ln == 0 {
		return ""
	}
	b, ok := g.mem.Read(ptr, ln)
	if !ok {
		panic("guest read out of range")
	}
	return string(b) // string() copies, so aliasing linear memory is safe
}
