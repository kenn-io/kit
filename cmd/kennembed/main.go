package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.kenn.io/kit/embedserver"
)

func main() {
	var opts embedserver.Options
	flag.StringVar(&opts.DataDir, "data-dir", "", "Model cache and runtime directory")
	flag.StringVar(&opts.Listen, "listen", "127.0.0.1:0", "Loopback listen address")
	flag.StringVar(&opts.Library, "runtime-library", "", "ONNX Runtime 1.23.2 library path")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	err := embedserver.Serve(ctx, opts)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
