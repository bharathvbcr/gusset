# X THREADS — Gusset

Three threads. Post the main one first; the other two work as follow-ups
later in the week so you're not spending the whole story in one day.

Image files are in `docs/img/linkedin/` (they work on X too).

---

## THREAD 1 — The six failures (main, post this one)

**1/**
A Go↔Rust binding takes twenty minutes to write.

It compiles. It returns the right answer. You loop it ten thousand times and it never flinches.

Then you put it behind an HTTP handler and point real concurrency at it.

Six ways it kills your process:

**2/**
The panic that kills you twice.

Since Rust 1.81 a panic escaping extern "C" aborts. So you wrap it in catch_unwind.

Then you turn the message into a C string:
CString::new(msg).unwrap()

NUL byte in the payload → Err → second panic → SIGABRT.

The safety net is what killed you.

**3/**
The thread storm.

To Go's scheduler a cgo call is a syscall. Block long enough and sysmon hands the P to a fresh OS thread.

Thread count tracks concurrency, not cores.

Default ceiling: 10,000

runtime: program exceeds 10000-thread limit
fatal error: thread exhaustion

**4/**
The 128 KiB trap.

A goroutine stack starts at 2 KiB and grows. A cgo call abandons that and runs on the host thread's stack.

glibc: usually 8 MiB (inherited from ulimit -s)
macOS secondary threads: 512 KiB
musl / Alpine: 128 KiB

A recursive parser eats that in microseconds.

**5/**
Worse: a Rust staticlib never runs std::rt::init, so Rust's stack-overflow handler is never installed.

Not for the calling thread. Not for threads the library spawns itself.

Bare SIGSEGV in production. No Go recovery. Nothing useful in the logs.

**6/**
The deadline that isn't one.

context.WithTimeout is completely inert across FFI. A goroutine inside foreign code cannot be preempted.

Client gave up at 500ms. You're burning CPU for 30 seconds.

You can't even pass a deadline — Go's monotonic clock and Rust's Instant don't compare.

**7/**
Two heaps, one limit.

GOMEMLIMIT exists so the GC collects before the OOM killer fires.

Go's GC cannot see one byte Rust allocated.

Go holds 1 GiB and thinks it has room. Rust takes 2.8. The cgroup sends SIGKILL while the collector is relaxed.

**8/**
The corpse that keeps taking traffic.

You survive a panic. But Rust panicked halfway through mutating a tree. Invariants broken, mutexes poisoned.

Your service returns a 500 and routes the next request to the same instance.

Now you're serving corruption with no obvious first cause.

**9/**
None of these is exotic.

Every one is the default behaviour of code that worked perfectly the day you wrote it.

cgo, cbindgen and uniffi solve type marshalling — and they're honest that it's all they claim.

Nobody owns the runtime seam underneath.

**10/**
So I built the thing that does.

Gusset: panic firewall, bounded pool, deadlines that cross, ABI verification, one memory budget across both heaps.

Raw cgo at 2,048 in-flight calls: 255 OS threads.
Same work through a bounded pool: 21.

[ATTACH 01-threads-vs-concurrency.png]

**11/**
It does not make your calls faster. On a single call it cannot beat raw cgo — 828× worse at a no-op, by construction.

By a few hundred µs of real work the curves converge.

Under ~10µs of work per call, don't use it. Use raw cgo.

[ATTACH 02-crossover-cost-per-call.png]

**12/**
Open source, MIT/Apache-2.0.

github.com/bharathvbcr/gusset

Full write-up — the six collisions in detail, the benchmarks, and which companies actually run Rust inside Go in production:

[ARTICLE LINK]

---

## THREAD 2 — The interop inversion (follow-up, spicier)

**1/**
Unpopular take after a year of moving off Python:

Python has better native interop than Go. It isn't close.

You leave Python to get closer to native code, and arrive holding worse tools for exactly that job.

**2/**
Python's side:

- a C-API that's a de facto standard, reimplemented by PyPy and GraalPy
- a stable ABI (abi3), so one wheel loads across minor versions
- abi3t landing in 3.15 for free-threaded builds
- PyO3 + maturin, mature and boring

**3/**
Shipping in production through that stack, today:

Polars
pydantic-core
tokenizers
cryptography
orjson

Millions of users. Nobody thinks about the boundary.

**4/**
Go's side: cgo. That's the list.

And cgo's costs are documented by its own maintainers, not its critics:

- strict pointer-pinning rules (cgocheck)
- a blocking call pins an OS thread
- needs a C toolchain, so cross-compilation needs zig cc or similar
- race detector and pprof degrade across the boundary

**5/**
Dave Cheney titled a post "cgo is not Go" in 2016.

Nobody has had to update it.

**6/**
In fairness: Go 1.26 cut cgo's baseline call overhead ~30%, and there's an open proposal to build cgo packages with no C toolchain at all.

The floor is rising.

But as of Go 1.27 no new FFI mechanism has shipped, and none is scheduled.

**7/**
That gap is why I spent weeks on a library that does nothing except make the boundary survivable.

Write-up: [ARTICLE LINK]

---

## THREAD 3 — Who actually does this (research, very shareable)

**1/**
I assumed lots of companies run Rust engines inside Go services in-process.

So I checked. The answer surprised me and it argues against the thing I built.

**2/**
Companies running both, and how the halves actually talk:

PingCAP (TiDB/TiKV) — gRPC
Linkerd — gRPC sidecar
Istio (ztunnel) — xDS over gRPC
AWS Firecracker — REST over a Unix socket
Fly.io — separate processes
Datadog — separate binary

[ATTACH 05-who-runs-both.png]

**3/**
Almost nobody does it in-process.

TiDB and TiKV — the canonical "Rust engine behind a Go service" — share a protobuf repo, not a header file.

PingCAP's own post on choosing Rust lists cgo overhead as a drawback of Go.

**4/**
The two verifiable in-process cgo cases both carry an asterisk:

InfluxData's libflux — real cgo, died with the InfluxDB 3 rewrite.

Dropbox's rust-brotli — live, real cgo, and compression is about the most forgiving payload you could pick.

**5/**
Where in-process Rust IS winning inside Go, the vehicle is Wasm, not FFI.

Arcjet rejected cgo on the record — they wanted CGO_ENABLED=0 and distroless images — and run Rust through wazero instead.

1Password's Go SDK does the same.

**6/**
The sharpest data point:

Datadog's Go agent already uses cgo to embed CPython. That team knows the cost and pays it willingly.

When they built a Rust data plane, they still chose a separate process.

**7/**
Honest reading: that scarcity is evidence about the cost of the boundary, not proof of an untapped gap.

Take the network hop if you can afford it. A socket buys a real fault domain.

My library is for when you can't.

[ARTICLE LINK]

---

## STANDALONE POSTS (space these out, no thread needed)

**A.**
Cloudflare lost ~6 hours last November to this line:

thread fl2_worker_thread panicked: called Result::unwrap() on an Err value

Nothing was memory-unsafe. Rust did exactly what it promises — refused to continue on an unexpected value.

Memory safety is not availability.

**B.**
The bug AI could not have found for me didn't live in Go and didn't live in Rust.

It lived where three true facts from three different docs touch:

- Rust 1.81 aborts on unwinding across extern "C"
- a panic inside a panic is fatal
- panic payloads can contain arbitrary bytes

**C.**
Generation is solved enough.

What isn't solved: knowing which three facts collide, which layer owns a failure nobody claimed, and what your benchmark is quietly lying to you about.

Go find the seams. That's where the work moved.

**D.**
Same work. Same machine. 2,048 concurrent calls.

Blocking cgo: 255 OS threads
Bounded pool: 21

Go's default ceiling is 10,000 and past it the runtime doesn't degrade, it dies.

[ATTACH 01-threads-vs-concurrency.png]

---

## NOTES

**Posting order.** Thread 1 now. Thread 3 two or three days later — it reads as
independent research rather than promotion, which travels further than a launch
post. Thread 2 last; it's the most argumentative and will pull the most replies,
so post it when you have time to sit in the thread and answer.

**Links.** X is widely reported to reduce distribution on posts containing
external links, and the usual workaround is to keep the link out of tweet 1 and
put it in the last tweet or a reply. I have not seen this confirmed by X in
anything I'd call authoritative, so treat it as cheap insurance rather than
established fact — the structure above costs you nothing either way.

**Images.** Charts do well on dev X. Tweet 10 and the standalone D both carry
the threads chart, so don't post them the same day.

**What will get pushed back on.** Tweet 4's macOS figure (512 KiB for secondary
threads — that's Apple's documented default, main thread is 8 MB), and the claim
in Thread 2 that Python's interop beats Go's. Both are defensible; have the
sources ready rather than arguing from memory.
