package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/bharathvbcr/gusset/internal/ltostrip"
)

// stripLTOMain runs `gussetseal -strip-lto`: either -o OUT.a IN.a, or -tree DIR.
// It returns the process exit code.
func stripLTOMain(out, tree string, args []string) int {
	ctx := context.Background()
	switch {
	case tree != "" && out == "" && len(args) == 0:
		n, err := ltostrip.StripTree(ctx, tree)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gussetseal: %v\n", err)
			return 1
		}
		fmt.Printf("gussetseal: stripped LTO sections from %d archive(s) under %s\n", n, tree)
		return 0
	case tree == "" && out != "" && len(args) == 1:
		res, err := ltostrip.Strip(ctx, args[0], out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gussetseal: %v\n", err)
			return 1
		}
		fmt.Printf("gussetseal: wrote %s: removed %v from %d of %d members\n", out, res.Removed, res.Stripped, res.Members)
		return 0
	}
	flag.Usage()
	return 2
}
