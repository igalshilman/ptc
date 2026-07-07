//! The cross-language boundary — the ONLY place with `#[no_mangle]`/`extern` and
//! raw linear-memory access. There is NO wasm import: the guest never calls back
//! into the host. Three exports: `guest_alloc`/`guest_dealloc` (the host writes the
//! script in and reads the output out) and `execute`.

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

/// Run one assembled script to synchronous quiescence and return its output blob.
/// The return value packs the blob location: `(ptr << 32) | len`. The host reads
/// those `len` bytes of JSON, then frees them with `guest_dealloc(ptr, len)`. The
/// blob is placed in a `guest_alloc` buffer so that free is symmetric.
#[no_mangle]
pub extern "C" fn execute(script_ptr: *const u8, script_len: i32) -> u64 {
    let out = engine::execute_script(unsafe { view(script_ptr, script_len) });
    let n = out.len();
    let buf = guest_alloc(n as i32);
    if n > 0 {
        unsafe { std::ptr::copy_nonoverlapping(out.as_ptr(), buf, n) };
    }
    ((buf as u64) << 32) | (n as u64)
}
