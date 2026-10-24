package benchfile

import (
	"strings"
	"testing"
)

// crates/gusset/benches/dispatch.rs prints one `BenchmarkDispatch/<arm>` line per
// round per arm, grouped by arm, with its execution order in `#` comment lines.
// The shape is what `cargo bench -p gusset --bench dispatch` emits (and what
// benchstat reads: it was run over a real recording of it while this was
// written); the numbers here are synthetic and mean nothing.
const rustDispatch = `# gusset-bench-rust-order: 2 rounds after a discarded round 0; each round ran every arm once; order alternates (odd rounds as listed, even rounds reversed)
# gusset-bench-rust-round-1: diagnostic_opcode0 registered_opcode0
# gusset-bench-rust-round-2: registered_opcode0 diagnostic_opcode0
BenchmarkDispatch/diagnostic_opcode0	5000	100.0 ns/op
BenchmarkDispatch/diagnostic_opcode0	5000	101.0 ns/op
BenchmarkDispatch/registered_opcode0	5000	102.0 ns/op
BenchmarkDispatch/registered_opcode0	5000	103.0 ns/op
`

// The cargo arm of record.sh puts this straight into a results file, so the
// parser every consumer of those files uses must accept it and keep each round
// as a sample.
func TestParseAcceptsTheRustDispatchBenchOutput(t *testing.T) {
	f, err := Parse(writeTemp(t, goodHeader+rustDispatch))
	if err != nil {
		t.Fatalf("the Rust dispatch bench output was rejected: %v", err)
	}
	for _, arm := range []string{"BenchmarkDispatch/diagnostic_opcode0", "BenchmarkDispatch/registered_opcode0"} {
		if n := len(f.ByName[arm]); n != 2 {
			t.Errorf("%s has %d samples, want one per round (2)", arm, n)
		}
	}
	if got := f.ByName["BenchmarkDispatch/registered_opcode0"][1]["ns/op"]; got != 103.0 {
		t.Errorf("second registered_opcode0 sample = %v, want 103", got)
	}
	if err := f.RequireProvenance(); err != nil {
		t.Errorf("provenance of the Rust arm's file: %v", err)
	}
}

// Why dispatch.rs groups its lines by arm instead of printing them as it runs:
// in execution order the arms alternate, and checkSingleWriter reads an arm that
// resumes after another as two processes writing one file. The interleaving is in
// the measurement, not in the file.
func TestParseRejectsRustDispatchLinesInExecutionOrder(t *testing.T) {
	inOrder := strings.Join([]string{
		"BenchmarkDispatch/diagnostic_opcode0\t5000\t100.0 ns/op",
		"BenchmarkDispatch/registered_opcode0\t5000\t102.0 ns/op",
		"BenchmarkDispatch/registered_opcode0\t5000\t103.0 ns/op",
		"BenchmarkDispatch/diagnostic_opcode0\t5000\t101.0 ns/op",
		"",
	}, "\n")
	mustReject(t, goodHeader+inOrder, "resumes")
}
