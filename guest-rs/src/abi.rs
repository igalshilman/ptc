//! The cross-language boundary — the ONLY place with `#[no_mangle]`/`extern` and
//! raw linear-memory access. There is still NO wasm import: the guest never calls
//! back into the host. The host drives the live program by calling these exports and
//! reading the step blob each returns:
//!
//!   - `guest_alloc` / `guest_dealloc` — host writes inputs in / reads outputs out.
//!   - `start(script)`               — begin a program; returns the first step.
//!   - `resolve(handle, jsonValue)`  — settle promise `handle`; returns the next step.
//!   - `reject(handle, message)`     — reject promise `handle`; returns the next step.
//!
//! Every step blob is `(ptr << 32) | len`; the host reads those `len` bytes of JSON
//! and frees them with `guest_dealloc(ptr, len)`.

use crate::engine;

/// View `len` bytes at `ptr` in linear memory (empty for null/zero-length). Valid
/// only for the duration of the call, during which the memory is stable.
unsafe fn view<'a>(ptr: *const u8, len: i32) -> &'a [u8] {
    if ptr.is_null() || len <= 0 {
        &[]
    } else {
        std::slice::from_raw_parts(ptr, len as usize)
    }
}

/// Copy an output blob into a fresh `guest_alloc` buffer and pack its location as
/// `(ptr << 32) | len` (symmetric with `guest_dealloc`).
fn pack(out: Vec<u8>) -> u64 {
    let n = out.len();
    let buf = guest_alloc(n as i32);
    if n > 0 {
        unsafe { std::ptr::copy_nonoverlapping(out.as_ptr(), buf, n) };
    }
    ((buf as u64) << 32) | (n as u64)
}

#[no_mangle]
pub extern "C" fn guest_alloc(size: i32) -> *mut u8 {
    if size <= 0 {
        return std::ptr::NonNull::<u8>::dangling().as_ptr();
    }
    let mut buf = Vec::<u8>::with_capacity(size as usize);
    let ptr = buf.as_mut_ptr();
    std::mem::forget(buf);
    ptr
}

#[no_mangle]
pub extern "C" fn guest_dealloc(ptr: *mut u8, size: i32) {
    if !ptr.is_null() && size > 0 {
        unsafe { drop(Vec::from_raw_parts(ptr, 0, size as usize)) };
    }
}

/// Begin a program: evaluate the assembled script and return the first step blob.
#[no_mangle]
pub extern "C" fn start(script_ptr: *const u8, script_len: i32) -> u64 {
    pack(engine::start(unsafe { view(script_ptr, script_len) }))
}

/// Settle the pending promise `handle` with the JSON value bytes, return next step.
#[no_mangle]
pub extern "C" fn resolve(handle: i32, value_ptr: *const u8, value_len: i32) -> u64 {
    pack(engine::resolve(handle, unsafe { view(value_ptr, value_len) }))
}

/// Reject the pending promise `handle` with the message bytes, return next step.
#[no_mangle]
pub extern "C" fn reject(handle: i32, msg_ptr: *const u8, msg_len: i32) -> u64 {
    pack(engine::reject(handle, unsafe { view(msg_ptr, msg_len) }))
}
