// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Command custom-hooks is hookinlet with a rate limit added, written the way an
// application that embeds the server would: it builds the same pieces the stock
// binary does, using only the public packages under pkg/, and passes its own
// inbox.Hooks to server.New.
//
// Run it with the same settings as hookinlet itself, plus two of its own:
//
//	HOOKINLET_ADMIN_TOKEN=...        at least 32 characters
//	HOOKINLET_MASTER_KEY=...         32 bytes, base64 (openssl rand -base64 32)
//	HOOKINLET_DATA_DIR=./data        (default)
//	HOOKINLET_LISTEN_ADDR=:8080      (default)
//	EXAMPLE_RATE=5                   deliveries a second to each inbox (default)
//	EXAMPLE_BURST=20                 how many may arrive at once (default)
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/server"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, os.Getenv); err != nil {
		log.Error("stopped", "err", err)
		stop()
		os.Exit(1)
	}
}

// run assembles the server and serves until ctx ends.
func run(ctx context.Context, log *slog.Logger, getenv func(string) string) error {
	adminToken := getenv("HOOKINLET_ADMIN_TOKEN")
	if len(adminToken) < 32 {
		return errors.New("HOOKINLET_ADMIN_TOKEN must be at least 32 characters")
	}
	key, err := base64.StdEncoding.DecodeString(getenv("HOOKINLET_MASTER_KEY"))
	if err != nil {
		return errors.New("HOOKINLET_MASTER_KEY must be base64")
	}
	box, err := secretbox.New(key) // checks the length
	if err != nil {
		return fmt.Errorf("HOOKINLET_MASTER_KEY: %w", err)
	}
	rate, err := number(getenv("EXAMPLE_RATE"), 5)
	if err != nil {
		return fmt.Errorf("EXAMPLE_RATE: %w", err)
	}
	burst, err := number(getenv("EXAMPLE_BURST"), 20)
	if err == nil && (burst < 1 || burst > 1e6 || burst != math.Trunc(burst)) {
		err = errors.New("must be a whole number from 1 to 1000000")
	}
	if err != nil {
		return fmt.Errorf("EXAMPLE_BURST: %w", err)
	}

	store, err := sqlite.Open(ctx, orDefault(getenv("HOOKINLET_DATA_DIR"), "./data"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	srv, err := server.New(server.Options{
		Addr:          orDefault(getenv("HOOKINLET_LISTEN_ADDR"), ":8080"),
		Logger:        log,
		Store:         store,
		SecretBox:     box,
		Notifier:      local.New(),
		Authenticator: token.NewAuthenticator(store, adminToken),
		Hooks:         NewRateLimiter(rate, int(burst)), // the one line that makes this different
	})
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func number(v string, def float64) (float64, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	// NaN and infinity parse without error and defeat every comparison.
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return 0, errors.New("must be a positive number")
	}
	return n, nil
}
