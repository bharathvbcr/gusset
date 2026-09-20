# Security Policy

Gusset provides the runtime boundary between Go and Rust in production. Its core mission is maintaining process stability, panic containment, and memory safety.

We take the security and integrity of this boundary seriously.

---

## Supported Versions

| Version | Supported |
| :--- | :--- |
| `v0.0.1` | :white_check_mark: |
| `< v0.0.1` | :x: |

---

## Reporting a Vulnerability

If you discover a security vulnerability—especially concerning:
- A panic payload or format string that escapes the `ffi_guard` and triggers `SIGABRT` or process crash.
- A memory corruption, use-after-free, or double-free across the FFI boundary.
- A Go pointer retention bug violating cgo pointer passing rules (`cgocheck`).
- A race condition in semaphore accounting or completion ticket dispatch that could lead to deadlock or unbounded thread exhaustion.

**Please do NOT report security vulnerabilities through public GitHub issues.**

Instead, report vulnerabilities privately via:
1. **GitHub Security Advisory:** Submit an advisory on the [Gusset Security tab](https://github.com/bharathvbcr/gusset/security/advisories/new).
2. **Email:** Contact the maintainer directly at `bharathchandra18@gmail.com`.

Please include:
- A clear description of the vulnerability.
- A minimal reproducible example (e.g. a failing test case in Go or Rust).
- The operating system, architecture, Go version, and Rust version used.

---

## Response Process

- We will acknowledge receipt of your vulnerability report within 48 hours.
- We will provide a detailed assessment and proposed mitigation timeline.
- Once a fix is verified against our full CI matrix (including ASan, Miri, and `-race`), we will release a security advisory and patched release.
