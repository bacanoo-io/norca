// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command norca-natsbridge forwards the event stream of a Norca region to
// NATS, and with -sync-pushes / -sync-regions syncs the pack registries on
// NATS messages. "norca-natsbridge notify-push" publishes a push notice.
// See the README of the natsbridge module.
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
	if len(os.Args) > 1 && os.Args[1] == "notify-push" {
		notifyPush(os.Args[2:])
		return
	}
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

// notifyPush publishes a push notice: packs were pushed to a registry
// source, the bridges with -sync-pushes sync the cluster registries of it.
func notifyPush(args []string) {
	cfg, notice, err := natsbridge.ParsePushConfig(args, os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "norca-natsbridge notify-push:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := natsbridge.NotifyPush(cfg, notice, logger); err != nil {
		fmt.Fprintln(os.Stderr, "norca-natsbridge notify-push:", err)
		os.Exit(1)
	}
}
