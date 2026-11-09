//go:build !linux

package gusset

import "os"

func pipeQueued(*os.File) string { return "not measured off linux" }
