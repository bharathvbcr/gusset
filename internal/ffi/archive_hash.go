package ffi

// This file is a stub, and nothing rewrites it.
//
// `go generate ./internal/ffi` (gen.go) writes the sha256 of the archive cgo
// links into zz_archive_hash.go beside it. That file is gitignored: its value
// describes one machine's archive, and while it was tracked here every rebuild
// dirtied the tree and merges conflicted on it. Its only job is to make
// the Go build cache recompile and relink this package when libgusset.a
// changes; nothing reads the constant. A checkout without it builds the same,
// and runs `go generate` after `cargo build` to get the cache invalidation.
//
// The stub stays because it carries no build tag: with cgo off or on windows,
// this package still has a file to compile, and unsupported.go turns that into
// the named refusal instead of a later missing symbol.
