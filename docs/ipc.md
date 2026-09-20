# Phase 4: Out-of-Process Crash Isolation Architecture (`gusset-ipc`)

As of 2026-09-20.

## Overview

In-process crash isolation (the Gusset runtime contract) protects against software panics, stack overflows, and concurrency faults. However, engines driving GPU hardware accelerators (such as Metal in `tessl` or CUDA kernels) can trigger hardware driver reset faults (e.g. GPU hang, memory fault in kernel space). When a driver fault occurs, the host operating system terminates the process with `SIGABRT` or `SIGKILL`, which no user-space signal handler or `catch_unwind` can survive.

For GPU workloads requiring zero-downtime serving from Go, Gusset defines the Phase 4 Out-of-Process module (`gusset-ipc`).

---

## Decision Record Summary (DECISIONS.md)

| Item | Choice | Rationale |
| :--- | :--- | :--- |
| **Transport** | `iceoryx2` upstream Go binding | Zero-copy shared memory IPC with formal distribution and maintenance from Eclipse Foundation. |
| **Adapter Role** | Thin adapter over iceoryx2 | Gusset avoids maintaining a custom shm transport. `gusset-ipc` provides the identical `gusset.Handle` Go API over IPC. |

---

## Architectural Model

```mermaid
flowchart TD
    subgraph GoProcess ["Go Service Process"]
        App["Go Application Handler"]
        IpcHandle["gusset-ipc.Handle\n(Identical Go API to gusset.Handle)"]
        IceClient["iceoryx2 Shm Client\n(Zero-copy request publisher & response subscriber)"]
        App --> IpcHandle
        IpcHandle --> IceClient
    end

    subgraph ShmTransport ["Shared Memory Layer (POSIX shm / memfd)"]
        ReqRing["Request Ring Buffer\n(Lock-free Zero-Copy Payloads)"]
        RespRing["Response Ring Buffer\n(Lock-free Zero-Copy Results)"]
        IceClient --> ReqRing
        RespRing --> IceClient
    end

    subgraph Supervisor ["Supervisor Daemon (e.g. tessld)"]
        Monitor["Process Monitor & Fork/Exec/Restart Controller"]
        Heartbeat["Microsecond Heartbeat Watcher\n(Watchdog thread)"]
        RestartBudget["Restart Budget & Circuit Breaker\n(N restarts within window T)"]
        Monitor --- Heartbeat
        Monitor --- RestartBudget
    end

    subgraph WorkerProcess ["Isolated Worker Subprocess (Metal / GPU Context)"]
        Worker["Rust Engine Worker Process"]
        GpuCtx["Metal / CUDA Kernel Context"]
        FaultZone["Fault Zone\n(Hardware GPU Hang / Driver Reset -> SIGKILL / SIGABRT)"]
        Worker --> GpuCtx
        GpuCtx --> FaultZone
    end

    ReqRing --> Worker
    Worker --> RespRing
    Monitor -. "fork / exec / signal monitoring" .-> Worker
```

## Fault Containment & Recovery Lifecycle

When driving GPU hardware, driver resets or kernel panics issue uncatchable `SIGKILL` or `SIGABRT` signals. An in-process cgo boundary cannot survive these faults; out-of-process crash isolation ensures the Go service continues uninterrupted:

```mermaid
sequenceDiagram
    autonumber
    participant Go as Go Service (gusset-ipc)
    participant Shm as iceoryx2 Shared Memory
    participant Super as Supervisor Daemon (tessld)
    participant Worker as Worker Subprocess (Metal/GPU)
    participant OS as Host OS / Kernel Driver

    Go->>Shm: Write request payload (zero-copy)
    Shm->>Worker: Worker reads request from ring buffer
    Worker->>Worker: Dispatch kernel to GPU (Metal/CUDA)

    critical Hardware Driver Reset Fault
        Worker->>OS: GPU Hang / Invalid Kernel Address
        OS->>Worker: Kernel driver terminates process (SIGABRT / SIGKILL)
        Note over Worker: Worker process dies immediately.<br/>In-process cgo would destroy entire Go process!
    end

    Note over Go: Go Service remains healthy & serving
    Super->>OS: Detect child worker termination via waitpid / signal
    Super->>Super: Check restart budget (<= N restarts in window T)
    Super->>Worker: Spawn fresh worker subprocess & re-init GPU context
    Worker->>Shm: Re-attach to shared memory rings
    Super-->>Go: Notify worker restored
    Go->>Shm: Retry transiently failed request
    Shm->>Worker: New worker processes request
    Worker->>Shm: Write response payload
    Shm-->>Go: Return result to Go caller
```

---

## Daemon Supervisor Protocol

1. **Shared Memory Ring:** Requests and responses pass through bounded lock-free shared memory channels.
2. **Heartbeats:** The supervisor exchanges microsecond heartbeats with worker processes.
3. **Fault Containment:** If a Metal kernel aborts the worker, only the worker sub-process terminates. The Go service remains alive and healthy.
4. **Restart Budget:** The supervisor restarts workers up to $N$ times within window $T$. If the budget is exhausted, circuit breaker opens.
5. **Transparency:** To the Go caller, `gusset-ipc.Handle` exposes the identical `Call(ctx, in)` and `Submit`/`Wait` semantics.
