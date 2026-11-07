package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"go.kenn.io/kit/lint/releasecheck"
)

func runReleaseArtifacts(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("kennlint release-artifacts", flag.ContinueOnError)
	flags.SetOutput(stderr)
	contractPath := flags.String("contract", "", "consumer JSON release contract (required)")
	dir := flags.String("dir", "dist", "final archives and SHA256SUMS directory")
	version := flags.String("version", "", "release version (required)")
	targetOS := flags.String("os", "", "explicit OS subset; omitted checks the entire inventory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *contractPath == "" || *version == "" {
		return errors.New("-contract and -version are required")
	}
	// Distinguish an omitted selector from an explicitly empty subset request.
	emptyOS := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "os" && f.Value.String() == "" {
			emptyOS = true
		}
	})
	if emptyOS {
		return errors.New("-os must select a nonempty declared OS subset")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(*contractPath)
	if err != nil {
		return fmt.Errorf("open contract: %w", err)
	}
	contract, err := releasecheck.ReadContract(f)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := releasecheck.Check(ctx, contract, *dir, *version, *targetOS); err != nil {
		return err
	}
	count := 0
	for _, a := range contract.Artifacts {
		if *targetOS == "" || a.OS == *targetOS {
			count++
		}
	}
	_, err = fmt.Fprintf(stdout, "checked %d release artifacts\n", count)
	return err
}
