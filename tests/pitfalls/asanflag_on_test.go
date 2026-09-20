//go:build asan

package pitfalls_test

// asanBuild reports whether this binary was built with `go test -asan`.
//
// Go defines the `asan` build constraint when the sanitizer is enabled, which is
// what lets the cross-free test tell "ASan watched and found nothing" apart from
// "ASan was never running" — two outcomes that must never share a result.
const asanBuild = true
