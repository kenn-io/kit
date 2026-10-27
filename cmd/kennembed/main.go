package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.kenn.io/kit/embedserver"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	err := embedserver.Serve(ctx, embedserver.Options{})
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
