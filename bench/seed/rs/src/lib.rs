use std::os::raw::c_char;
use std::ffi::CString;
use std::panic::{catch_unwind, AssertUnwindSafe};

#[no_mangle]
pub extern "C" fn rs_noop(x: u64) -> u64 { std::hint::black_box(x).wrapping_add(1) }

// batched: sum-and-square each element in place, returns count
#[no_mangle]
pub unsafe extern "C" fn rs_batch(ptr: *mut u64, len: usize) -> u64 {
    let s = std::slice::from_raw_parts_mut(ptr, len);
    let mut acc = 0u64;
    for v in s.iter_mut() { *v = v.wrapping_mul(*v).wrapping_add(1); acc = acc.wrapping_add(*v); }
    acc
}

#[repr(C)]
pub struct FfiStatus { pub code: i32, pub error_msg: *mut c_char }

// The document's guard, verbatim in spirit (unwrap on CString::new).
#[no_mangle]
pub unsafe extern "C" fn rs_guarded_doc(mode: i32, out_status: *mut FfiStatus) -> u64 {
    let mut out: u64 = 0;
    match catch_unwind(AssertUnwindSafe(|| {
        if mode == 1 { panic!("plain panic"); }
        if mode == 2 { panic!("panic with embedded NUL \0 byte"); }
        42u64
    })) {
        Ok(v) => { out = v; (*out_status).code = 0; (*out_status).error_msg = std::ptr::null_mut(); }
        Err(err) => {
            (*out_status).code = -1;
            let msg = if let Some(s) = err.downcast_ref::<&str>() { *s }
                      else if let Some(s) = err.downcast_ref::<String>() { s.as_str() }
                      else { "Unknown Rust panic" };
            (*out_status).error_msg = CString::new(msg).unwrap().into_raw();  // <- doc's bug
        }
    }
    out
}

#[no_mangle]
pub unsafe extern "C" fn rs_free_string(p: *mut c_char) { if !p.is_null() { drop(CString::from_raw(p)); } }
