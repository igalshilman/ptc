//! Minimal rquickjs guest — feasibility spike for wasm32-wasip1.
//!
//! Goal: prove rquickjs (QuickJS) compiles to wasm32-wasip1, loads in wazero, and
//! evaluates JS. Not the real ABI — just enough to retire the three unknowns
//! (wasip1+wazero works, binary size, hermetic offline build).

use rquickjs::{Context, Runtime};

/// Eval `6 * 7` in a fresh QuickJS runtime+context and return the result.
#[no_mangle]
pub extern "C" fn eval_answer() -> i32 {
    let rt = Runtime::new().expect("runtime");
    let ctx = Context::full(&rt).expect("context");
    ctx.with(|ctx| ctx.eval::<i32, _>("6 * 7").expect("eval"))
}
