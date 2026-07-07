package agent

import _ "embed"

// guestWasm is the compiled QuickJS guest, embedded so the binary is
// self-contained. It is host-agnostic: it knows nothing about Restate or the tools
// layered on top. It is a committed, prebuilt artifact — rebuild it with
// `make guest-rs` (Rust/rquickjs) when guest-rs/ changes.
//
//go:generate make -C .. guest-rs
//go:embed quickjs_guest.wasm
var guestWasm []byte
