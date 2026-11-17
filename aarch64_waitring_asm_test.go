//go:build arm64

package gusset

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestWaitRingARM64MachineCodePreservesDekkerBarriers closes the source-to-ISA
// compiler-lowering gap between the `aarch64.cat` memory-model proof and the
// emitted binary by compiling the real package under both GOARM64=v8.0 (LL/SC
// baseline) and GOARM64=v8.4 (LSE + FEAT_LRCPC) and inspecting the 32-bit ARM64
// machine words of `(*ticketReader).waitRing`:
//
//  1. `ringReady()` and `overflowPending()` must lower to `LDAR` (encoding
//     `0xc8dffc00` mask `0xfffffc00`, `Exp & R & A` in `aarch64.cat`), NEVER
//     bare `LDAPR` (`0xb8bfc000` / `0xf8bfc000`, `Exp & R & Q`), which lacks
//     StoreLoad ordering after `STLR`/`STLXR`.
//  2. `atomic.SwapUint32(waiting, 1)` must emit `SWPALW` (`0xb8e08000` mask
//     `0xffe0fc00`) on `GOARM64=v8.4` and `LDAXRW` (`0x885ffc00`) + `STLXRW`
//     (`0x8800fc00` mask `0xffe0fc00`) on `GOARM64=v8.0`.
func TestWaitRingARM64MachineCodePreservesDekkerBarriers(t *testing.T) {
	hexWordRe := regexp.MustCompile(`\t0x[0-9a-f]+\t+([0-9a-f]{8})\t`)

	disassemble := func(t *testing.T, goarm64 string) []uint32 {
		t.Helper()
		binPath := filepath.Join(t.TempDir(), "gusset_"+goarm64+".test")
		buildCmd := exec.Command("go", "test", "-c", "-o", binPath, ".")
		buildCmd.Env = append(os.Environ(), "GOARM64="+goarm64)
		if out, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("go test -c (GOARM64=%s) failed: %v\n%s", goarm64, err, out)
		}

		dumpCmd := exec.Command("go", "tool", "objdump", "-s", `\(\*ticketReader\)\.waitRing$`, binPath)
		raw, err := dumpCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go tool objdump (GOARM64=%s) failed: %v\n%s", goarm64, err, raw)
		}

		var words []uint32
		for _, line := range strings.Split(string(raw), "\n") {
			m := hexWordRe.FindStringSubmatch(line)
			if len(m) != 2 {
				continue
			}
			val, err := strconv.ParseUint(m[1], 16, 32)
			if err != nil {
				t.Fatalf("parse instruction word %q: %v", m[1], err)
			}
			words = append(words, uint32(val))
		}
		if len(words) == 0 {
			t.Fatalf("GOARM64=%s: objdump returned 0 instruction words for (*ticketReader).waitRing:\n%s", goarm64, raw)
		}
		return words
	}

	isLDAR64 := func(w uint32) bool { return (w & 0xfffffc00) == 0xc8dffc00 }
	isLDAPR := func(w uint32) bool {
		return (w&0xfffffc00) == 0xb8bfc000 || (w&0xfffffc00) == 0xf8bfc000
	}
	isSWPALW := func(w uint32) bool { return (w & 0xffe0fc00) == 0xb8e08000 }
	isLDAXRW := func(w uint32) bool { return (w & 0xfffffc00) == 0x885ffc00 }
	isSTLXRW := func(w uint32) bool { return (w & 0xffe0fc00) == 0x8800fc00 }
	isPlainSTLRW := func(w uint32) bool { return (w & 0xfffffc00) == 0x889ffc00 }

	for _, arch := range []string{"v8.0", "v8.4"} {
		t.Run("GOARM64="+arch, func(t *testing.T) {
			words := disassemble(t, arch)
			var ldarCount, ldaprCount, swpalwCount, ldaxrwCount, stlxrwCount, stlrwCount int
			for _, w := range words {
				if isLDAR64(w) {
					ldarCount++
				}
				if isLDAPR(w) {
					ldaprCount++
				}
				if isSWPALW(w) {
					swpalwCount++
				}
				if isLDAXRW(w) {
					ldaxrwCount++
				}
				if isSTLXRW(w) {
					stlxrwCount++
				}
				if isPlainSTLRW(w) {
					stlrwCount++
				}
			}

			if ldarCount < 2 {
				t.Fatalf("GOARM64=%s: expected >= 2 64-bit LDAR instructions in waitRing (ringReady + overflowPending), found %d", arch, ldarCount)
			}
			if ldaprCount != 0 {
				t.Fatalf("GOARM64=%s: waitRing emitted %d FEAT_LRCPC LDAPR instructions; bare LDAPR breaks StoreLoad Dekker ordering after STLR/STLXR", arch, ldaprCount)
			}
			if stlrwCount != 0 {
				t.Fatalf("GOARM64=%s: waitRing emitted %d plain STLRW instructions; waiting must use atomic RMW SwapUint32", arch, stlrwCount)
			}

			switch arch {
			case "v8.4":
				if swpalwCount < 2 {
					t.Fatalf("GOARM64=v8.4: expected >= 2 LSE SWPALW instructions (set waiting=1 and clear waiting=0), found %d", swpalwCount)
				}
			case "v8.0":
				if swpalwCount == 0 && (ldaxrwCount < 2 || stlxrwCount < 2) {
					t.Fatalf("GOARM64=v8.0: expected either runtime-gated SWPALW or >= 2 LDAXRW/STLXRW pairs, got swpalw=%d ldaxrw=%d stlxrw=%d",
						swpalwCount, ldaxrwCount, stlxrwCount)
				}
			}
		})
	}
}
