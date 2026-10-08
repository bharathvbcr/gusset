//! The Rust-owned buffer registry (R16).

use super::sys::RawBuffer;
use super::{lock_recover, reserve_id, Handle, JobResult, MAX_BUFFER_BYTES, MAX_INLINE_INPUT};
use std::sync::atomic::Ordering;
use std::sync::Arc;

/// Registry entry for one Rust-owned buffer.
pub(super) struct BufferSlot {
    pub(super) buf: Arc<RawBuffer>,
    /// Set once the buffer has become a job's output.
    ///
    /// Go frees an output buffer when its waiter is done with it. Two results
    /// naming one buffer therefore free it under each other: the drain loop
    /// takes both completions back to back and both resolve the same pointer.
    /// An engine returning a captured, pre-allocated id does exactly that on
    /// its second call, so a buffer may become an output once per lifetime.
    output_claimed: bool,
    /// Set when the pointer was handed to Go (`NewBuffer` / `gusset_buf_alloc`).
    ///
    /// Go still holds a view. The result path frees an output buffer when the
    /// waiter is done, which would release this memory under that view.
    caller_held: bool,
}

impl BufferSlot {
    fn new(buf: RawBuffer, output_claimed: bool, caller_held: bool) -> Self {
        Self {
            buf: Arc::new(buf),
            output_claimed,
            caller_held,
        }
    }
}

impl Handle {
    /// Allocates 64-byte aligned Rust-owned buffer memory (R16).
    ///
    /// The pointer is not published to Go. An engine may return this id as its
    /// output once. Buffers Go can already see are [`Handle::buf_alloc_published`].
    pub fn buf_alloc(&self, len: usize) -> Result<(u64, *mut u8), String> {
        self.allocate_buffer(len, false)
    }

    /// Allocates a buffer and records that Go holds the pointer (`NewBuffer`).
    ///
    /// `gusset_buf_alloc` is this path. An engine that returns the id as
    /// `JobOutput::Buffer` is handing back memory the caller still views; the
    /// result path would free it when the waiter finishes.
    pub fn buf_alloc_published(&self, len: usize) -> Result<(u64, *mut u8), String> {
        self.allocate_buffer(len, true)
    }

    fn allocate_buffer(&self, len: usize, caller_held: bool) -> Result<(u64, *mut u8), String> {
        if self.poisoned.load(Ordering::Acquire) {
            return Err("handle is poisoned".to_string());
        }
        if self.closed.load(Ordering::Acquire) {
            return Err("handle is closed".to_string());
        }
        let id = reserve_id(&self.next_buffer_id)?;
        let buf = RawBuffer::allocate(len)?;
        let ptr = buf.as_mut_ptr();

        let mut map = lock_recover(&self.buffers);
        map.insert(id, BufferSlot::new(buf, false, caller_held));

        Ok((id, ptr))
    }

    /// Copies `data` into a new buffer and returns its id and pointer.
    ///
    /// The memcpy runs on the calling thread. Workers use this to promote a
    /// large `JobResult::Ok` off the cgo take path, and `gusset_take` uses it for
    /// the small results it still copies (R16 egress).
    ///
    /// Deliberately not poison-checked, unlike [`Handle::buf_alloc`]. Poison
    /// refuses new work (I2); this carries out a result that already exists. A
    /// check here lost a sibling's finished result the moment another job
    /// panicked, and only for results small enough not to have been promoted.
    pub(crate) fn buf_from_bytes(&self, data: &[u8]) -> Result<(u64, *mut u8), String> {
        if self.closed.load(Ordering::Acquire) {
            return Err("handle is closed".to_string());
        }
        let id = reserve_id(&self.next_buffer_id)?;
        let buf = RawBuffer::from_bytes(data)?;
        let ptr = buf.as_mut_ptr();
        // Born as an output: nothing else may return it as one.
        lock_recover(&self.buffers).insert(id, BufferSlot::new(buf, true, false));
        Ok((id, ptr))
    }

    /// Registers an engine's `BufferAlloc` output as a result buffer, zero-copy.
    ///
    /// Runs on the worker after the engine's own `catch_unwind`, under the
    /// worker's outer firewall. An empty output is an empty
    /// result; one over the 1 GiB ceiling is refused (and freed) exactly as a
    /// `Vec<u8>` of that size is; memory that cannot be adopted as-is is copied.
    #[cfg(gusset_allocator_api)]
    pub(super) fn adopt_output(&self, out: Vec<u8, crate::alloc::BufferAlloc>) -> JobResult {
        if out.is_empty() {
            return JobResult::Ok(Vec::new());
        }
        if out.len() > MAX_BUFFER_BYTES {
            return JobResult::Err(format!(
                "output {} bytes exceeds maximum {} bytes",
                out.len(),
                MAX_BUFFER_BYTES
            ));
        }
        if self.closed.load(Ordering::Acquire) {
            return JobResult::Err("handle is closed".to_string());
        }
        let id = match reserve_id(&self.next_buffer_id) {
            Ok(id) => id,
            Err(e) => return JobResult::Err(e),
        };
        let buf = match RawBuffer::adopt(out) {
            Ok(buf) => buf,
            Err(foreign) => match RawBuffer::from_bytes(&foreign) {
                Ok(buf) => buf,
                Err(e) => return JobResult::Err(e),
            },
        };
        // Born as an output, like buf_from_bytes: nothing may return it again.
        lock_recover(&self.buffers).insert(id, BufferSlot::new(buf, true, false));
        JobResult::Buffer(id)
    }

    /// Transfers a live buffer to a job's result, refusing any second owner.
    ///
    /// Refused when the buffer is already some result's output, when Go still
    /// holds the pointer (`caller_held`), or when another work unit still holds
    /// it as its input. In each case the waiter freeing this result would
    /// release memory someone else is reading. Checked under the registry lock,
    /// which is also where `submit` clones an input's `Arc`, so the strong
    /// count cannot grow between the check and the claim.
    pub(super) fn claim_output(&self, id: u64) -> Result<(), String> {
        let mut map = lock_recover(&self.buffers);
        let slot = map.get_mut(&id).ok_or_else(|| {
            format!(
                "engine returned buffer id {} as its output: no such live buffer",
                id
            )
        })?;
        if slot.output_claimed {
            return Err(format!(
                "engine returned buffer id {} as its output: it is already another call's \
                 output; allocate a new buffer for each result",
                id
            ));
        }
        // A published buffer still has a Go view. Moving it to a result would
        // let the waiter free it under that view — the same use-after-free as
        // returning the input, for every buffer `NewBuffer` handed out rather
        // than only the one this unit was given. Vale refuses the move while
        // another owner is live; this is that check, stored at alloc time
        // because Rust cannot see the Go pointer.
        if slot.caller_held {
            return Err(format!(
                "engine returned buffer id {} as its output: the caller still holds it; \
                 allocate a new buffer for the result",
                id
            ));
        }
        if Arc::strong_count(&slot.buf) > 1 {
            return Err(format!(
                "engine returned buffer id {} as its output: another work unit is still \
                 reading it as its input",
                id
            ));
        }
        slot.output_claimed = true;
        Ok(())
    }

    /// Looks up a live buffer by id, returning its mutable pointer and byte length.
    pub fn buf_get(&self, id: u64) -> Result<(*mut u8, usize), String> {
        let map = lock_recover(&self.buffers);
        if let Some(slot) = map.get(&id) {
            Ok((slot.buf.as_mut_ptr(), slot.buf.len()))
        } else {
            Err(format!("buffer id {} not found", id))
        }
    }

    /// Frees a Rust-owned buffer by id (R4, R16).
    ///
    /// Missing ids are success: `Free` and the `AddCleanup` backstop can race, and
    /// treating a second free as an error turns a safety net into a user-visible
    /// failure on the path that already released the memory.
    pub fn buf_free(&self, id: u64) -> Result<(), String> {
        let mut map = lock_recover(&self.buffers);
        map.remove(&id);
        Ok(())
    }
}

/// Moves a large `JobResult::Ok` onto a Buffer so `gusset_take` does not
/// memcpy on the cgo thread.
///
/// The worker holds an upgraded `Arc`, not `&self`, so this is a function on
/// `&Handle` rather than a method.
pub(super) fn materialize_result(handle: &Handle, result: JobResult) -> JobResult {
    match result {
        JobResult::Ok(data) if data.len() > MAX_BUFFER_BYTES => JobResult::Err(format!(
            "output {} bytes exceeds maximum {} bytes",
            data.len(),
            MAX_BUFFER_BYTES
        )),
        JobResult::Ok(data) if data.len() > MAX_INLINE_INPUT => {
            match handle.buf_from_bytes(&data) {
                Ok((id, _)) => JobResult::Buffer(id),
                Err(e) => JobResult::Err(e),
            }
        }
        other => other,
    }
}
