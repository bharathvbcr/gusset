package main

import (
	"context"
	"debug/macho"
	"path/filepath"
	"strings"
	"testing"
)

// On ELF the architecture is the configured LD's own, so this file is
// darwin-only: on Linux the test does not exist rather than reporting a skip.

// The driver used to run without -arch, so it assumed the host's architecture:
// an x86_64 archive sealed on an arm64 Mac produced an object with none of the
// archive in it. Both slices are checked, so the test means the same on either
// host. A universal archive is refused rather than sealed for one slice.
func TestSealKeepsTheArchiveArchitecture(t *testing.T) {
	need(t, "cc", "lipo")
	want := map[string]macho.Cpu{"x86_64": macho.CpuAmd64, "arm64": macho.CpuArm64}
	for arch, cpu := range want {
		t.Run(arch, func(t *testing.T) {
			dir := t.TempDir()
			in := archive(t, dir, engineC, "-arch", arch)
			out := filepath.Join(dir, "sealed.a")
			if err := seal(in, "ea_", out); err != nil {
				t.Fatalf("seal: %v", err)
			}
			f, err := macho.Open(filepath.Join(dir, "sealed.o"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if f.Cpu != cpu {
				t.Fatalf("sealed object is %v, want %v", f.Cpu, cpu)
			}
		})
	}

	dir := t.TempDir()
	x := archive(t, mkdirAll(t, dir, "x"), engineC, "-arch", "x86_64")
	a := archive(t, mkdirAll(t, dir, "a"), engineC, "-arch", "arm64")
	fat := filepath.Join(dir, "libfat.a")
	if err := run(context.Background(), "lipo", "-create", x, a, "-output", fat); err != nil {
		t.Fatal(err)
	}
	err := seal(fat, "ea_", filepath.Join(dir, "sealed.a"))
	if err == nil || !strings.Contains(err.Error(), "2 architectures") {
		t.Fatalf("seal of a universal archive: err = %v, want a refusal naming both architectures", err)
	}
}
