package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"kotantei/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := app.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv)
	stop()
	os.Exit(code)
}
