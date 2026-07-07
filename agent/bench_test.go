package agent

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
)

// These benchmarks answer the "is instance reuse/pooling worth it?" question
// offline: how expensive is a FRESH wazero instance per Run, both on its own and
// as a fraction of a whole guest round — and how much allocator/GC pressure many
// concurrent sessions generate. They deliberately EXCLUDE the LLM call, which
// dominates a real agent round (~1–5 s); compare the numbers below against that.

func benchEngine(b *testing.B) (*Engine, context.Context) {
	b.Helper()
	ctx := context.Background()
	eng, err := NewEngine(ctx, guestWasm)
	if err != nil {
		b.Fatalf("engine: %v", err)
	}
	b.Cleanup(func() { _ = eng.Close(ctx) })
	return eng, ctx
}

// BenchmarkInstantiateOnly: a bare InstantiateModule + Close — the wazero per-Run
// cost with NO JS_NewRuntime/eval. This is the floor a pool could remove.
func BenchmarkInstantiateOnly(b *testing.B) {
	eng, ctx := benchEngine(b)
	cfg := wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mod, err := eng.runtime.InstantiateModule(ctx, eng.compiled, cfg)
		if err != nil {
			b.Fatal(err)
		}
		_ = mod.Close(ctx)
	}
}

// BenchmarkRunProgramTrivial: the FULL per-round guest cost — instantiate +
// a fresh QuickJS runtime/context (created inside execute) + eval a trivial program +
// close. This is what actually happens each agent round, minus the LLM call.
func BenchmarkRunProgramTrivial(b *testing.B) {
	eng, ctx := benchEngine(b)
	inv := &testInvoker{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sb := NewSandbox(eng, inv)
		if _, err := sb.RunProgram(ctx, "return 1 + 1;"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRunProgramParallel: concurrent sessions, each a fresh Sandbox off the
// shared Engine (mirrors concurrent Agent/<session>/Ask). Surfaces throughput and
// the allocator/GC pressure of churning 256 MiB-capped instances — the likeliest
// real motivation for pooling, more than single-round latency.
func BenchmarkRunProgramParallel(b *testing.B) {
	eng, ctx := benchEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		inv := &testInvoker{}
		for pb.Next() {
			sb := NewSandbox(eng, inv)
			if _, err := sb.RunProgram(ctx, "return 1 + 1;"); err != nil {
				b.Fatal(err)
			}
		}
	})
}
