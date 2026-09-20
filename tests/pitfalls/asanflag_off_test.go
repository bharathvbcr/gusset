//go:build !asan

package pitfalls_test

// asanBuild is false in an ordinary build; see asanflag_on_test.go.
const asanBuild = false
