//! Worker pool and Handle lifecycle with bounded concurrency and signal protection (I4, I5).

pub mod sys;

use crate::alloc::{record_alloc, record_dealloc};
use crate::ffi::guard::{extract_panic_payload, install_panic_hook};
use crate::header::{CallHeader, CancelReason, JobContext};
use std::collections::HashMap;
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender};
use std::sync::{Arc, Mutex, RwLock};
use std::thread;
use sys::RawBuffer;

/// Work unit sent to worker threads.
struct WorkUnit {
    ticket: u64,
    ctx: JobContext,
    input: Vec<u8>,
}

/// Result of job execution.
#[derive(Debug)]
pub enum JobResult {
    /// Succeeded with output bytes.
    Ok(Vec<u8>),
    /// Returned error.
    Err(String),
    /// Panic caught by firewall.
    Panic(String),
    /// Job cancelled or timed out.
    Cancelled(CancelReason),
}

/// Engine handler function type.
pub type EngineFn = Box<dyn Fn(&JobContext, &[u8]) -> Result<Vec<u8>, String> + Send + Sync + 'static>;

static GLOBAL_ENGINE: RwLock<Option<EngineFn>> = RwLock::new(None);

/// Sets the global engine execution handler.
pub fn set_engine_handler<F>(f: F)
where
    F: Fn(&JobContext, &[u8]) -> Result<Vec<u8>, String> + Send + Sync + 'static,
{
    if let Ok(mut w) = GLOBAL_ENGINE.write() {
        *w = Some(Box::new(f));
    }
}

/// Default execution dispatcher for testing and baseline operations.
pub fn default_dispatch(ctx: &JobContext, input: &[u8]) -> Result<Vec<u8>, String> {
    // If a global engine is set, delegate to it.
    {
        if let Ok(r) = GLOBAL_ENGINE.read() {
            if let Some(ref engine) = *r {
                return engine(ctx, input);
            }
        }
    }

    // Default engine handling test modes.
    if input.is_empty() {
        return Ok(Vec::new());
    }

    match input[0] {
        // Mode 0: Echo input back
        0 => Ok(input.to_vec()),
        // Mode 1: Plain panic
        1 => panic!("plain panic"),
        // Mode 2: Panic with embedded NUL byte
        2 => panic!("panic with embedded NUL \0 byte"),
        // Mode 3: Non-string panic
        3 => std::panic::panic_any(42i32),
        // Mode 4: Error with panic in Display / formatting
        4 => {
            struct PanicOnDisplay;
            impl std::fmt::Display for PanicOnDisplay {
                fn fmt(&self, _f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                    panic!("panic inside Display implementation");
                }
            }
            Err(PanicOnDisplay.to_string())
        }
        // Mode 5: Timeout / sleep loop checking ctx.check()
        5 => {
            let iterations = if input.len() >= 2 { input[1] as usize } else { 100 };
            for _ in 0..iterations {
                ctx.check().map_err(|e| format!("cancelled: {:?}", e))?;
                thread::sleep(std::time::Duration::from_millis(10));
            }
            Ok(vec![5, 0])
        }
        // Mode 6: Deep recursion to prove stack size (R8)
        6 => {
            fn recurse(depth: u32) -> u32 {
                if depth == 0 {
                    0
                } else {
                    std::hint::black_box(recurse(depth - 1) + 1)
                }
            }
            let depth = if input.len() >= 5 {
                u32::from_le_bytes([input[1], input[2], input[3], input[4]])
            } else {
                50_000
            };
            let res = recurse(depth);
            Ok(res.to_le_bytes().to_vec())
        }
        // Mode 10: Vector sum-and-square computation (Phase 2 CPU-bound engine)
        10 => {
            let mut acc = 0u64;
            for (i, &b) in input[1..].iter().enumerate() {
                if i % 1024 == 0 {
                    ctx.check().map_err(|e| format!("cancelled: {:?}", e))?;
                }
                let val = b as u64;
                acc = acc.wrapping_add(val.wrapping_mul(val));
            }
            Ok(acc.to_le_bytes().to_vec())
        }
        // Default: echo
        _ => Ok(input.to_vec()),
    }
}

/// Represents an active Gusset runtime handle (I4).
pub struct Handle {
    pool_size: usize,
    pipe_write_fd: i32,
    poisoned: AtomicBool,
    closed: AtomicBool,
    sender: SyncSender<WorkUnit>,
    results: Mutex<HashMap<u64, JobResult>>,
    cancel_flags: Mutex<HashMap<u64, Arc<AtomicBool>>>,
    buffers: Mutex<HashMap<u64, RawBuffer>>,
    next_ticket: AtomicU64,
    next_buffer_id: AtomicU64,
    workers: Mutex<Vec<thread::JoinHandle<()>>>,
    receiver: Arc<Mutex<Receiver<WorkUnit>>>,
}

impl Handle {
    /// Opens a new handle with a dedicated worker pool of pool_size threads.
    pub fn open(pool_size: u32, pipe_write_fd: i32) -> Result<Arc<Self>, String> {
        install_panic_hook();

        let pool_size = if pool_size == 0 { 4 } else { pool_size as usize };
        let (sender, receiver) = sync_channel(pool_size * 2);
        let receiver = Arc::new(Mutex::new(receiver));

        let handle = Arc::new(Self {
            pool_size,
            pipe_write_fd,
            poisoned: AtomicBool::new(false),
            closed: AtomicBool::new(false),
            sender,
            results: Mutex::new(HashMap::new()),
            cancel_flags: Mutex::new(HashMap::new()),
            buffers: Mutex::new(HashMap::new()),
            next_ticket: AtomicU64::new(1),
            next_buffer_id: AtomicU64::new(1),
            workers: Mutex::new(Vec::with_capacity(pool_size)),
            receiver,
        });

        // Spawn pool worker threads
        handle.spawn_workers(pool_size)?;

        Ok(handle)
    }

    fn spawn_workers(self: &Arc<Self>, count: usize) -> Result<(), String> {
        let mut workers = self.workers.lock().map_err(|e| e.to_string())?;

        for i in 0..count {
            let handle_clone = Arc::clone(self);
            let receiver_clone = Arc::clone(&self.receiver);
            let worker_id = i;

            // Spawn worker with 8 MiB explicit stack size (R8) and install sigaltstack
            let builder = thread::Builder::new()
                .name(format!("gusset-w{}", worker_id))
                .stack_size(8 * 1024 * 1024);

            let join_handle = builder
                .spawn(move || {
                    sys::install_sigaltstack();

                    loop {
                        let unit = {
                            let rx = receiver_clone.lock();
                            match rx {
                                Ok(guard) => match guard.recv() {
                                    Ok(u) => u,
                                    Err(_) => break, // Channel closed, shutdown
                                },
                                Err(_) => break,
                            }
                        };

                        // Check cancellation before starting
                        let early_cancel = unit.ctx.check();
                        let result = match early_cancel {
                            Err(reason) => JobResult::Cancelled(reason),
                            Ok(()) => {
                                // Execute behind catch_unwind
                                match catch_unwind(AssertUnwindSafe(|| {
                                    default_dispatch(&unit.ctx, &unit.input)
                                })) {
                                    Ok(Ok(out)) => JobResult::Ok(out),
                                    Ok(Err(err)) => JobResult::Err(err),
                                    Err(payload) => {
                                        // Caught panic: poison handle (I2)
                                        handle_clone.poisoned.store(true, Ordering::Release);
                                        let msg = extract_panic_payload(payload);
                                        JobResult::Panic(msg)
                                    }
                                }
                            }
                        };

                        // Store result
                        if let Ok(mut map) = handle_clone.results.lock() {
                            map.insert(unit.ticket, result);
                        }

                        // Remove cancel flag
                        if let Ok(mut flags) = handle_clone.cancel_flags.lock() {
                            flags.remove(&unit.ticket);
                        }

                        // Write 8-byte ticket to pipe fd (wakes Go netpoller)
                        let _ = sys::write_ticket(handle_clone.pipe_write_fd, unit.ticket);
                    }
                })
                .map_err(|e| format!("failed to spawn worker thread: {}", e))?;

            workers.push(join_handle);
        }

        Ok(())
    }

    /// Submits a task to the pool (R16).
    pub fn submit(
        &self,
        header: CallHeader,
        input: &[u8],
        buffer_id: u64,
    ) -> Result<u64, String> {
        if self.poisoned.load(Ordering::Acquire) {
            return Err("handle is poisoned".to_string());
        }
        if self.closed.load(Ordering::Acquire) {
            return Err("handle is closed".to_string());
        }

        // Determine input payload:
        // R16: inputs up to 4 KiB copied during submit; larger inputs live in Buffer
        let task_input = if buffer_id > 0 {
            let buffers = self.buffers.lock().map_err(|e| e.to_string())?;
            let rec = buffers.get(&buffer_id).ok_or_else(|| {
                format!("buffer id {} not found", buffer_id)
            })?;
            rec.to_vec()
        } else {
            input.to_vec()
        };

        let ticket = self.next_ticket.fetch_add(1, Ordering::Relaxed);
        let cancel_flag = Arc::new(AtomicBool::new(false));

        if let Ok(mut map) = self.cancel_flags.lock() {
            map.insert(ticket, Arc::clone(&cancel_flag));
        }

        let ctx = JobContext::new(header, cancel_flag);
        let unit = WorkUnit {
            ticket,
            ctx,
            input: task_input,
        };

        self.sender.send(unit).map_err(|e| e.to_string())?;

        Ok(ticket)
    }

    /// Moves out the result of a completed job (moves out exactly once).
    pub fn take(&self, ticket: u64) -> Result<JobResult, String> {
        let mut results = self.results.lock().map_err(|e| e.to_string())?;
        results
            .remove(&ticket)
            .ok_or_else(|| format!("ticket {} not found or already taken", ticket))
    }

    /// Cancels a specific job by ticket (I3).
    pub fn cancel(&self, ticket: u64) {
        if let Ok(flags) = self.cancel_flags.lock() {
            if let Some(flag) = flags.get(&ticket) {
                flag.store(true, Ordering::Release);
            }
        }
    }

    /// Cancels all currently pending jobs (I3).
    pub fn cancel_all(&self) {
        if let Ok(flags) = self.cancel_flags.lock() {
            for flag in flags.values() {
                flag.store(true, Ordering::Release);
            }
        }
    }

    /// Allocates 64-byte aligned Rust-owned buffer memory (R16).
    pub fn buf_alloc(&self, len: usize) -> Result<(u64, *mut u8), String> {
        let buf = RawBuffer::allocate(len)?;
        let ptr = buf.as_mut_ptr();
        record_alloc(len);

        let id = self.next_buffer_id.fetch_add(1, Ordering::Relaxed);
        let mut map = self.buffers.lock().map_err(|e| e.to_string())?;
        map.insert(id, buf);

        Ok((id, ptr))
    }

    /// Frees a Rust-owned buffer by id (R4, R16).
    pub fn buf_free(&self, id: u64) -> Result<(), String> {
        let mut map = self.buffers.lock().map_err(|e| e.to_string())?;
        if let Some(buf) = map.remove(&id) {
            record_dealloc(buf.len());
            Ok(())
        } else {
            Err(format!("buffer id {} not found", id))
        }
    }

    /// Closes the handle, cancels in-flight jobs, and closes write fd.
    pub fn close(&self) {
        if !self.closed.swap(true, Ordering::SeqCst) {
            self.cancel_all();
            sys::close_fd(self.pipe_write_fd);
        }
    }

    /// Checks whether the handle is currently poisoned.
    pub fn is_poisoned(&self) -> bool {
        self.poisoned.load(Ordering::Acquire)
    }

    /// Returns pool size.
    pub fn pool_size(&self) -> usize {
        self.pool_size
    }
}

impl Drop for Handle {
    fn drop(&mut self) {
        self.close();
    }
}
