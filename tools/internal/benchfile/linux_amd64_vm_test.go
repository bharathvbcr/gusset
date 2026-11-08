package benchfile_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/gusset/tools/internal/benchfile"
)

var linuxVmFiles = []string{
	"bench/results/linux-amd64-vm/inline-after.txt",
	"bench/results/linux-amd64-vm/inline-before-a4e8f0b.txt",
	"bench/results/linux-amd64-vm/ring-crossover-after.txt",
	"bench/results/linux-amd64-vm/ring-crossover-before-907ecb1.txt",
	"bench/results/linux-amd64-vm/ring-main-after.txt",
	"bench/results/linux-amd64-vm/ring-main-before-907ecb1.txt",
	"bench/results/linux-amd64-vm/seed-micro.txt",
	"bench/results/linux-amd64-vm/simd-ring-multiversion.txt",
	"bench/results/linux-amd64-vm/simd.txt",
	"bench/results/linux-amd64-vm/waitchan-after.txt",
	"bench/results/linux-amd64-vm/waitchan-before-2b3b32c.txt",
}

// Find repository root from current test package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root with go.mod")
		}
		dir = parent
	}
}

// Helper: parse only provenance headers into a benchfile.File.
func parseProvenanceHeadersOnly(content string) (*benchfile.File, error) {
	var headerLines []string
	count, variants := 0, 0
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "# gusset-bench") {
			headerLines = append(headerLines, line)
		}
		if v, ok := strings.CutPrefix(line, "# gusset-bench-count:"); ok {
			count, _ = strconv.Atoi(strings.TrimSpace(v))
		}
		if strings.HasPrefix(line, "# gusset-bench-variant-") {
			variants++
		}
	}
	// Dummy rows so the structural parse succeeds, as many as the header's
	// count promises: RequireProvenance compares the two.
	cleanRows := strings.Repeat("BenchmarkDummy/Arm1/noop-4 1000 100 ns/op\n", max(count, 1)*max(variants, 1))
	full := strings.Join(headerLines, "\n") + "\n" + cleanRows
	tmp, err := os.CreateTemp("", "prov_test_*.txt")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(full); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()
	return benchfile.Parse(tmp.Name())
}

// 1. Verify that all 11 Linux VM benchmark data files carry valid provenance headers.
func TestAdversarial_All11LinuxVmFilesValidProvenance(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range linuxVmFiles {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			path := filepath.Join(root, rel)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", rel, err)
			}
			f, err := parseProvenanceHeadersOnly(string(data))
			if err != nil {
				t.Fatalf("parseProvenanceHeadersOnly(%s) failed: %v", rel, err)
			}
			if err := f.RequireProvenance(); err != nil {
				t.Fatalf("RequireProvenance(%s) failed: %v", rel, err)
			}
			if f.Env == nil {
				t.Fatalf("Env is nil for %s", rel)
			}
			if !f.Env.Exclusive {
				t.Fatalf("Expected exclusive: yes for %s", rel)
			}
			if f.Env.MinIdle <= 0 {
				t.Fatalf("Expected positive min-idle for %s, got %v", rel, f.Env.MinIdle)
			}
			if f.Env.IdleBefore <= 0 || f.Env.IdleAfter <= 0 {
				t.Fatalf("Expected positive idle before/after for %s, got %v / %v", rel, f.Env.IdleBefore, f.Env.IdleAfter)
			}
		})
	}
}

// 2. Empirical verification: Structurally contiguous benchmark files pass full Parse + RequireProvenance.
func TestAdversarial_StructurallyContiguousFilesPassFullParse(t *testing.T) {
	root := repoRoot(t)
	contiguousFiles := []string{
		"bench/results/linux-amd64-vm/seed-micro.txt",
		"bench/results/linux-amd64-vm/simd-ring-multiversion.txt",
		"bench/results/linux-amd64-vm/simd.txt",
	}

	for _, rel := range contiguousFiles {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			path := filepath.Join(root, rel)
			f, err := benchfile.Parse(path)
			if err != nil {
				t.Fatalf("Parse(%s) failed: %v", rel, err)
			}
			if err := f.RequireProvenance(); err != nil {
				t.Fatalf("RequireProvenance(%s) failed: %v", rel, err)
			}
		})
	}
}

// 3. Empirical verification: Adversarial corruption of provenance headers is strictly caught across ALL 11 files.
func TestAdversarial_LinuxVmFilesRejectCorruptedProvenance(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range linuxVmFiles {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			path := filepath.Join(root, rel)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", rel, err)
			}
			content := string(data)

			// Attack 1: Remove all provenance headers
			t.Run("missing_provenance", func(t *testing.T) {
				var strippedLines []string
				for _, line := range strings.Split(content, "\n") {
					if !strings.HasPrefix(line, "# gusset-bench") {
						strippedLines = append(strippedLines, line)
					}
				}
				f, err := parseProvenanceHeadersOnly(strings.Join(strippedLines, "\n"))
				if err != nil {
					t.Fatalf("Parse failed: %v", err)
				}
				if err := f.RequireProvenance(); err == nil {
					t.Fatalf("RequireProvenance should have rejected stripped provenance in %s", rel)
				} else if !strings.Contains(err.Error(), "no provenance header") {
					t.Fatalf("Expected 'no provenance header' error, got %v", err)
				}
			})

			// Attack 2: Corrupt exclusive to "no"
			t.Run("corrupt_exclusive_no", func(t *testing.T) {
				corrupt := strings.Replace(content, "# gusset-bench-exclusive: yes", "# gusset-bench-exclusive: no", 1)
				f, err := parseProvenanceHeadersOnly(corrupt)
				if err != nil {
					t.Fatalf("Parse failed: %v", err)
				}
				if err := f.RequireProvenance(); err == nil {
					t.Fatalf("RequireProvenance should have rejected exclusive: no in %s", rel)
				} else if !strings.Contains(err.Error(), "without exclusive use") {
					t.Fatalf("Expected 'without exclusive use' error, got %v", err)
				}
			})

			// Attack 3: Corrupt cpu-idle-after below floor
			t.Run("corrupt_idle_below_floor", func(t *testing.T) {
				corrupt := content
				for _, line := range strings.Split(content, "\n") {
					if strings.HasPrefix(line, "# gusset-bench-cpu-idle-after:") {
						corrupt = strings.Replace(content, line, "# gusset-bench-cpu-idle-after: 5.0", 1)
						break
					}
				}
				f, err := parseProvenanceHeadersOnly(corrupt)
				if err != nil {
					t.Fatalf("Parse failed: %v", err)
				}
				if err := f.RequireProvenance(); err == nil {
					t.Fatalf("RequireProvenance should have rejected idle below floor in %s", rel)
				} else if !strings.Contains(err.Error(), "below the") {
					t.Fatalf("Expected 'below the' floor error, got %v", err)
				}
			})

			// Attack 4: Remove min-idle floor line
			t.Run("missing_min_idle_floor", func(t *testing.T) {
				var strippedLines []string
				for _, line := range strings.Split(content, "\n") {
					if !strings.HasPrefix(line, "# gusset-bench-min-idle:") {
						strippedLines = append(strippedLines, line)
					}
				}
				f, err := parseProvenanceHeadersOnly(strings.Join(strippedLines, "\n"))
				if err != nil {
					t.Fatalf("Parse failed: %v", err)
				}
				if err := f.RequireProvenance(); err == nil {
					t.Fatalf("RequireProvenance should have rejected missing min-idle floor in %s", rel)
				} else if !strings.Contains(err.Error(), "does not say what idle floor") {
					t.Fatalf("Expected 'does not say what idle floor' error, got %v", err)
				}
			})

			// Attack 5: Remove cpu-idle-before line
			t.Run("missing_cpu_idle_before", func(t *testing.T) {
				var strippedLines []string
				for _, line := range strings.Split(content, "\n") {
					if !strings.HasPrefix(line, "# gusset-bench-cpu-idle-before:") {
						strippedLines = append(strippedLines, line)
					}
				}
				f, err := parseProvenanceHeadersOnly(strings.Join(strippedLines, "\n"))
				if err != nil {
					t.Fatalf("Parse failed: %v", err)
				}
				if err := f.RequireProvenance(); err == nil {
					t.Fatalf("RequireProvenance should have rejected missing cpu-idle-before in %s", rel)
				} else if !strings.Contains(err.Error(), "no CPU idle reading") {
					t.Fatalf("Expected 'no CPU idle reading' error, got %v", err)
				}
			})

			// Attack 6: Corrupt cpu-idle-before with non-numeric value
			t.Run("corrupt_non_numeric_idle", func(t *testing.T) {
				for _, line := range strings.Split(content, "\n") {
					if strings.HasPrefix(line, "# gusset-bench-cpu-idle-before:") {
						corrupt := strings.Replace(content, line, "# gusset-bench-cpu-idle-before: notanumber", 1)
						f, err := parseProvenanceHeadersOnly(corrupt)
						if err != nil {
							t.Fatalf("Parse failed: %v", err)
						}
						if err := f.RequireProvenance(); err == nil {
							t.Fatalf("RequireProvenance should have rejected non-numeric idle in %s", rel)
						} else if !strings.Contains(err.Error(), "no CPU idle reading") {
							t.Fatalf("Expected 'no CPU idle reading' error, got %v", err)
						}
						break
					}
				}
			})
		})
	}
}
