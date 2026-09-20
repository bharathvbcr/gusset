package pitfalls_test

// R4's deliberate cross-free, which the nightly ASan job named and did not have.
//
// The job used to build under the sanitizer and run nothing; it now runs the suite,
// and this is the negative test that gives running it meaning. Without a violation
// that ASan is *supposed* to catch, a green ASan run only proves the code did not
// happen to trip the sanitizer — not that the sanitizer was watching.
//
// Two halves, and neither is allowed to look like the other:
//
//   - Under `-asan`, freeing Rust memory with libc free must be detected. If it is
//     not, the sanitizer is not instrumenting the allocator and every other ASan
//     result that night is worthless.
//   - Without `-asan`, the test reports SKIP. It does not pass. `go test -asan` is
//     unsupported on darwin/arm64, so on a developer Mac this check genuinely
//     cannot run, and saying so is the only honest outcome.

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/bharathvbcr/gusset"
	"github.com/bharathvbcr/gusset/tests/cgoprobe"
)

// crossFreeEnv re-enters the test binary as the violating child.
const crossFreeEnv = "GUSSET_TEST_CROSS_FREE"

func TestR4_CrossFreeIsDetectedUnderASan(t *testing.T) {
	if os.Getenv(crossFreeEnv) == "1" {
		crossFreeChild()
		os.Exit(0)
	}

	if !asanBuild {
		t.Skipf("built without -asan (unsupported on %s/%s), so the allocator "+
			"mismatch cannot be detected here; the nightly Linux ASan job runs this. "+
			"R4 is still gated on every commit by gussetvet's static C.free rule.",
			runtime.GOOS, runtime.GOARCH)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestR4_CrossFreeIsDetectedUnderASan$")
	cmd.Env = append(os.Environ(), crossFreeEnv+"=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	if err == nil {
		t.Fatalf("freeing Rust-allocated memory with libc free was not detected under "+
			"ASan; the sanitizer is not instrumenting the allocator, so every other "+
			"ASan result in this run is uninformative.\nchild output:\n%s", out.String())
	}

	// ASan words this differently depending on which allocator made the block and
	// which free ran first, so accept the family rather than pinning one string
	// and failing on a wording change.
	text := out.String()
	wanted := []string{
		"attempting double-free",
		"double-free",
		"alloc-dealloc-mismatch",
		"attempting free on address which was not malloc",
		"AddressSanitizer",
	}
	for _, w := range wanted {
		if strings.Contains(text, w) {
			t.Logf("ASan detected the cross-free: %s", w)
			return
		}
	}
	t.Fatalf("child died, but not with an ASan allocator report, so this proves "+
		"nothing about R4.\nerror: %v\noutput:\n%s", err, text)
}

// crossFreeChild allocates through Gusset and frees through libc.
func crossFreeChild() {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		panic(err)
	}
	// Not deferred: the whole point is that this process is expected to die inside
	// the free below, and a deferred Close would not run anyway.

	buf, err := h.NewBuffer(4096)
	if err != nil {
		panic(err)
	}
	b := buf.Bytes()
	if len(b) == 0 {
		panic("empty buffer")
	}

	// R4 violation: this memory came from Rust's allocator with 64-byte alignment,
	// which on Linux means posix_memalign underneath.
	cgoprobe.FreeWithLibcFree(unsafe.Pointer(&b[0]))
	runtime.KeepAlive(buf)

	// The violation alone may not be reportable on its own — freeing a
	// posix_memalign block with free() is legal C, so ASan has nothing to object
	// to yet. R4's actual damage lands here, when the owning allocator releases a
	// block that is already gone. That is the double free ASan reports, and it is
	// why "whoever allocated it frees it" is a rule rather than a style preference.
	_ = buf.Free()
	_ = h.Close()
}
