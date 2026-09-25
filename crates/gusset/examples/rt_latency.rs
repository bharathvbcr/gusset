use gusset::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use gusset::pool::Handle;
use std::time::Instant;
fn main() {
    let mut fds = [0i32; 2];
    unsafe { libc::pipe(fds.as_mut_ptr()) };
    let h = Handle::open(
        std::env::var("POOL")
            .ok()
            .and_then(|v| v.parse().ok())
            .unwrap_or(4),
        fds[1],
    )
    .unwrap_or_else(|e| panic!("{e}"));
    let hdr = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let n: u32 = std::env::var("N")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(20000);
    let t0 = Instant::now();
    for _ in 0..n {
        let t = h.submit(hdr, &[0], 0).unwrap_or_else(|e| panic!("{e}"));
        let mut b = [0u8; 8];
        unsafe { libc::read(fds[0], b.as_mut_ptr() as *mut _, 8) };
        assert_eq!(u64::from_ne_bytes(b), t);
        let _ = h.take(t);
    }
    println!("rust-only round trip: {:?}/op", t0.elapsed() / n);
}
