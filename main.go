package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	charmLog "github.com/charmbracelet/log"

	app "github.com/se0wtf/go-garmindives/internal/app"
)

func main() {
	logger, err := app.NewAppLogger(os.Stderr, os.Getenv("LOG_LEVEL"))
	if err != nil {
		charmLog.Error("invalid logger configuration", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, logger); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("interrupted")
			return
		}
		logger.Error("application failed", "err", err)
		os.Exit(1)
	}
}
