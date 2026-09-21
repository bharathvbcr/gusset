package main

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/gusset/tools/internal/benchfile"
)

// The structural checks moved to tools/internal/benchfile, which is where both
// benchplot and benchdoc now reach them. What remains here is what only
// benchplot can know: the physics of the two transports it charts.

func ns(name string, values ...float64) (string, []benchfile.Sample) {
	out := make([]benchfile.Sample, 0, len(values))
	for _, v := range values {
		out = append(out, benchfile.Sample{"ns/op": v})
	}
	return name, out
}

func dataset(pairs ...func() (string, []benchfile.Sample)) map[string][]benchfile.Sample {
	m := map[string][]benchfile.Sample{}
	for _, p := range pairs {
		k, v := p()
		m[k] = v
	}
	return m
}

func TestCalibratedFromName(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantIters int
		wantLo    float64
		wantHi    float64
		wantOK    bool
	}{
		// The interval, not the point: label() truncates, so "709us" means
		// the loop took somewhere in [709, 710) µs.
		{"BenchmarkCrossoverSerial/RawCgo/1000000it-709us", 1000000, 709000, 710000, true},
		{"BenchmarkCrossoverSerial/Gusset/1000it-708ns", 1000, 708, 709, true},
		{"BenchmarkCrossoverSerial/RawCgo/noop", 0, 0, 0, false},
		{"BenchmarkThreadScaling/RawCgo/conc512", 0, 0, 0, false},
	} {
		iters, c, ok := calibratedFromName(tc.name)
		if ok != tc.wantOK || iters != tc.wantIters || c.lo != tc.wantLo || c.hi != tc.wantHi {
			t.Errorf("calibratedFromName(%q) = (%d, [%v,%v), %v), want (%d, [%v,%v), %v)",
				tc.name, iters, c.lo, c.hi, ok, tc.wantIters, tc.wantLo, tc.wantHi, tc.wantOK)
		}
	}
}

// The four arms of a real certified recording labelled the identical 6.9 µs
// loop as both "6us" and "7us", because Duration.Microseconds() truncates.
// Comparing the printed numbers makes that look like a 17% disagreement and
// fails a recording that is in fact clean — which this check did, on the first
// honest dataset it ever saw.
func TestCalibrationAgreementToleratesLabelTruncation(t *testing.T) {
	m := dataset(
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/RawCgo/10000it-7us", 7195)
		},
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/Gusset/10000it-7us", 18870)
		},
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverParallel/RawCgo/10000it-7us", 426)
		},
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverParallel/Gusset/10000it-6us", 4255)
		},
	)
	if err := checkCalibrationAgreement(m); err != nil {
		t.Fatalf("rounding between 6us and 7us rejected as disagreement: %v", err)
	}
}

// The real clean pair. Two arms, recorded minutes apart in separate processes,
// calibrating the identical loop at 709 µs and 705 µs. A check that fails this
// fails every honest recording.
func TestCalibrationAgreementAcceptsArmsThatAgree(t *testing.T) {
	m := dataset(
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/RawCgo/1000000it-709us", 715455)
		},
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/Gusset/1000000it-705us", 764013)
		},
	)
	if err := checkCalibrationAgreement(m); err != nil {
		t.Fatalf("arms 0.6%% apart rejected: %v", err)
	}
}

// The signal the previous version of this file worked around instead of
// reading. A contaminated arm calibrated the same deterministic loop at 875 µs
// where a quiet machine measures 728 µs — and because calibrate() takes the
// minimum of five attempts, a 20% high minimum means all five were saturated.
func TestCalibrationAgreementRejectsContaminatedArm(t *testing.T) {
	m := dataset(
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/RawCgo/1000000it-728us", 715455)
		},
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/Gusset/1000000it-875us", 764013)
		},
	)
	err := checkCalibrationAgreement(m)
	if err == nil {
		t.Fatal("arms disagreeing by 20% about a deterministic loop were accepted")
	}
	for _, want := range []string{"disagree by 20%", "728µs", "875µs", "deterministic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A single arm cannot disagree with itself, and the scaling files contain only
// one. A false positive there would fail `make docs-check` on honest data.
func TestCalibrationAgreementAcceptsASingleArm(t *testing.T) {
	m := dataset(func() (string, []benchfile.Sample) {
		return ns("BenchmarkCrossoverSerial/RawCgo/1000000it-709us", 715455)
	})
	if err := checkCalibrationAgreement(m); err != nil {
		t.Fatalf("single arm rejected: %v", err)
	}
}

// Serial Gusset runs the identical Rust loop plus a semaphore, a submit, a
// worker dispatch, a pipe write and a netpoller wake. It cannot come out
// faster, so when it does, the blocking-cgo arm was measured under load the
// Gusset arm did not see.
func TestTransportOrderingRejectsImpossibleResult(t *testing.T) {
	m := dataset(
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/RawCgo/1000000it-709us", 2980000)
		},
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/Gusset/1000000it-709us", 764000)
		},
	)
	works := []workPoint{{iters: 1000000, label: "709 µs"}}
	err := checkTransportOrdering(m, works)
	if err == nil {
		t.Fatal("a chart showing Gusset faster than the call it wraps was accepted")
	}
	if !strings.Contains(err.Error(), "the transport cannot do") {
		t.Errorf("unexpected error: %v", err)
	}
}

// The real clean pair again: Gusset at 1.07x blocking cgo on 709 µs of work is
// the expected ordering, not a failure.
func TestTransportOrderingAcceptsRealMeasurement(t *testing.T) {
	m := dataset(
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/RawCgo/1000000it-709us", 715455)
		},
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/Gusset/1000000it-709us", 764013)
		},
	)
	works := []workPoint{{iters: 1000000, label: "709 µs"}}
	if err := checkTransportOrdering(m, works); err != nil {
		t.Fatalf("real measurement rejected: %v", err)
	}
}
