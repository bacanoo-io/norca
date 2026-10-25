// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command norca-natsbridge forwards the event stream of a Norca region to
// NATS. See the README of the natsbridge module.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hashicorp/nomad/natsbridge"
)

func main() {
	cfg, err := natsbridge.ParseConfig(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "norca-natsbridge:", err)
		os.Exit(2)
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "norca-natsbridge: invalid -log-level:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := natsbridge.Run(ctx, cfg, logger); err != nil {
		logger.Error("norca-natsbridge failed", "error", err)
		os.Exit(1)
	}
}
