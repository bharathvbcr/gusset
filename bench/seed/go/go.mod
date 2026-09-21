module ffibench

go 1.26

// Benchmark-only module. It links libffibench.a for the raw-cgo baseline and
// imports Gusset for the other half of the same measurement, which is why it is
// a module of its own: neither dependency belongs in the main module's build.
require github.com/bharathvbcr/gusset v0.0.0

replace github.com/bharathvbcr/gusset => ../../..
