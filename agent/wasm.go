package agent

import _ "embed"

// guestWasm is the compiled QuickJS reactor (guest/guest.c), embedded so the
// binary is self-contained. It is host-agnostic: it knows nothing about Restate
// or the tools layered on top. It is a committed, prebuilt artifact — rebuild it
// from the C source with `make guest` (needs Docker) only when guest.c changes.
//
//go:generate make -C .. guest
//go:embed quickjs_guest.wasm
var guestWasm []byte
