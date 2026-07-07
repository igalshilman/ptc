//! The cross-language boundary — and the ONLY place with `extern`/`#[no_mangle]`
//! and raw linear-memory access. The single wasm import (`env.host_call`) and every
//! wasm export live here; each export is a thin marshaling wrapper over
//! [`crate::engine`]. Keeping the wire here means the logic in `engine` is ordinary,
//! testable Rust with no `unsafe` FFI.

use crate::engine;

// ── The one host import ─────────────────────────────────────────────────────────

#[link(wasm_import_module = "env")]
extern "C" {
    // Registers a named host call and returns a handle (negative = refused).
    #[link_name = "host_call"]
    fn raw_host_call(name: *const u8, name_len: i32, arg: *const u8, arg_len: i32) -> i32;
}

/// Safe wrapper the engine calls to register a host operation.
pub(crate) fn host_call(name: &str, arg: &str) -> i32 {
    unsafe { raw_host_call(name.as_ptr(), name.len() as i32, arg.as_ptr(), arg.len() as i32) }
}

// ── Raw linear-memory helper ─────────────────────────────────────────────────────

/// View `len` bytes at `ptr` in linear memory (empty for null/zero-length). The
/// borrow is only used for the duration of the host call, during which the memory
/// is stable.
unsafe fn view<'a>(ptr: *const u8, len: i32) -> &'a [u8] {
    if ptr.is_null() || len <= 0 {
        &[]
    } else {
        std::slice::from_raw_parts(ptr, len as usize)
    }
}

// ── Memory management (the host allocs buffers it writes into / reads from) ──────

#[no_mangle]
pub extern "C" fn guest_alloc(size: i32) -> *mut u8 {
    // A zero-length request gets a non-null dangling pointer (never read/written by
    // the host), so it allocates nothing — symmetric with guest_dealloc's no-op.
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

// ── Evaluate / drive ─────────────────────────────────────────────────────────────

#[no_mangle]
pub extern "C" fn eval_code(code: *const u8, code_len: i32) -> i32 {
    engine::eval(unsafe { view(code, code_len) })
}

#[no_mangle]
pub extern "C" fn run_microtasks() -> i32 {
    engine::run_microtasks()
}

#[no_mangle]
pub extern "C" fn resolve_handle(handle: i32, value: *const u8, value_len: i32, is_error: i32) {
    let value = String::from_utf8_lossy(unsafe { view(value, value_len) }).into_owned();
    engine::resolve(handle, &value, is_error != 0);
}

// ── Pending host calls ───────────────────────────────────────────────────────────

#[no_mangle]
pub extern "C" fn get_pending_count() -> i32 {
    engine::pending_count()
}

#[no_mangle]
pub extern "C" fn get_pending_handles(out: *mut i32, capacity: i32) -> i32 {
    if out.is_null() || capacity <= 0 {
        return 0;
    }
    let slice = unsafe { std::slice::from_raw_parts_mut(out, capacity as usize) };
    engine::fill_pending_handles(slice) as i32
}

// ── Result / error readback ──────────────────────────────────────────────────────

#[no_mangle]
pub extern "C" fn get_result_ptr() -> *const u8 {
    engine::result_ptr()
}
#[no_mangle]
pub extern "C" fn get_result_len() -> i32 {
    engine::result_len()
}
#[no_mangle]
pub extern "C" fn get_result_is_error() -> i32 {
    engine::result_is_error()
}
#[no_mangle]
pub extern "C" fn get_error_ptr() -> *const u8 {
    engine::error_ptr()
}
#[no_mangle]
pub extern "C" fn get_error_len() -> i32 {
    engine::error_len()
}

#[no_mangle]
pub extern "C" fn cleanup() {
    engine::cleanup()
}
