//go:build !cgo || windows

package ffi

// archive_hash.go has no build tag, so this package would otherwise compile
// when cgo is off or GOOS is windows and the refusal would be a later missing
// symbol. The conversion is a type error on purpose: the compiler prints it.
// A comment does not.
var _ int = "gusset requires cgo on a unix target; see docs/platforms.md"
