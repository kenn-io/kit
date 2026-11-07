//go:build ignore

// Verify final kennembed archives, then prepare a relocated native parity input.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/kit/internal/embedassets"
	"go.kenn.io/kit/lint/releasecheck"
	"go.kenn.io/kit/selfupdate"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := verifyRelease(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func verifyRelease(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify-kennembed-release", flag.ContinueOnError)
	flags.SetOutput(stderr)
	contractPath := flags.String("contract", ".github/kennembed-release.json", "reviewed artifact contract")
	dir := flags.String("dir", "dist", "final archive directory")
	version := flags.String("version", "", "release version or tag")
	targetOS := flags.String("os", "", "explicit matrix OS subset; empty requires all targets")
	prepare := flags.Bool("prepare-native", false, "extract verified native archive and export parity input through GITHUB_ENV")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *version == "" {
		return fmt.Errorf("-version is required and positional arguments are not accepted")
	}
	file, err := os.Open(*contractPath)
	if err != nil {
		return err
	}
	contract, err := releasecheck.ReadContract(file)
	_ = file.Close()
	if err != nil {
		return err
	}
	// Check never discovers expectations from the output directory. Generate the
	// checksum payload from the same explicit inventory; a missing archive fails.
	var checksums strings.Builder
	var native *releasecheck.Artifact
	count := 0
	for _, artifact := range contract.Artifacts {
		if *targetOS != "" && artifact.OS != *targetOS {
			continue
		}
		name := strings.ReplaceAll(artifact.Name, "{version}", strings.TrimPrefix(*version, "v"))
		if filepath.Base(name) != name || strings.ContainsAny(name, "\\\r\n") {
			return fmt.Errorf("invalid archive basename %q", name)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		digest, err := selfupdate.HashFile(filepath.Join(*dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&checksums, "%s  %s\n", digest, name)
		count++
		if artifact.OS == runtime.GOOS && artifact.Arch == runtime.GOARCH {
			candidate := artifact
			native = &candidate
		}
	}
	if err := os.WriteFile(filepath.Join(*dir, "SHA256SUMS"), []byte(checksums.String()), 0o644); err != nil {
		return err
	}
	if err := releasecheck.Check(ctx, contract, *dir, *version, *targetOS); err != nil {
		return err
	}
	if *prepare {
		if native == nil {
			return fmt.Errorf("contract subset has no native %s/%s archive", runtime.GOOS, runtime.GOARCH)
		}
		envFile := os.Getenv("GITHUB_ENV")
		if envFile == "" {
			return fmt.Errorf("GITHUB_ENV is required with -prepare-native")
		}
		name := strings.ReplaceAll(native.Name, "{version}", strings.TrimPrefix(*version, "v"))
		if err := prepareNative(filepath.Join(*dir, name), native.Executable, os.Getenv("RUNNER_TEMP"), envFile); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "verified %d final kennembed release archives\n", count)
	return nil
}

func prepareNative(archivePath, executableName, tempParent, envFile string) error {
	root, err := os.MkdirTemp(tempParent, "kennembed-release-*")
	if err != nil {
		return err
	}
	// Keep the directory for the subsequent parity step. The ephemeral runner
	// owns its cleanup; tests place it under t.TempDir.
	var extractErr error
	if strings.HasSuffix(archivePath, ".zip") {
		extractErr = selfupdate.ExtractZip(archivePath, root)
	} else {
		extractErr = selfupdate.ExtractTarGz(archivePath, root)
	}
	if extractErr != nil {
		return extractErr
	}
	executable := filepath.Join(root, filepath.FromSlash(executableName))
	library := filepath.Join(filepath.Dir(executable), embedassets.LibraryName())
	for _, path := range []string{executable, library} {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("extracted native payload is not a nonempty regular file: %s", path)
		}
	}
	file, err := os.OpenFile(envFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(file, "KIT_EMBED_TEST_EXECUTABLE=%s\nKIT_EMBED_TEST_LIBRARY=%s\n", executable, library)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
