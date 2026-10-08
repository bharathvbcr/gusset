module r14host

go 1.26

// Spike-only module. It links two engine archives next to Gusset's, which is
// why it is a module of its own: neither engine belongs in the main build.
require github.com/bharathvbcr/gusset v0.0.0

replace github.com/bharathvbcr/gusset => ../../..
