// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// main.go: process entry point.  Sets up structured logging, parses the
// environment-driven Config (see config.go), constructs a Handler (see
// handler.go), and serves it.

package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/MatusOllah/slogcolor"
	"github.com/livekit/protocol/auth"
	"github.com/mattn/go-isatty"
)

func main() {
	opts := slogcolor.DefaultOptions
	opts.NoColor = !isatty.IsTerminal(os.Stderr.Fd())

	logLevelString := os.Getenv("LIVEKIT_LOG_LEVEL")
	switch strings.ToLower(logLevelString) {
	case "debug":
		opts.Level = slog.LevelDebug
	case "info":
	case "warn", "warning":
		opts.Level = slog.LevelWarn
	case "error":
		opts.Level = slog.LevelError
	case "":
		opts.Level = slog.LevelInfo
		slog.Info("log level defaulting to info")
	default:
		opts.Level = slog.LevelInfo
		slog.Warn("Invalid log level in LIVEKIT_LOG_LEVEL, defaulting to info",
			"invalidValue", logLevelString)
	}
	slog.SetDefault(slog.New(slogcolor.NewHandler(os.Stderr, opts)))

	config, err := parseConfig()
	if err != nil {
		log.Fatal(err)
	}

	handler := NewHandler(
		LiveKitAuth{
			key:          config.Key,
			secret:       config.Secret,
			authProvider: auth.NewSimpleKeyProvider(config.Key, config.Secret),
			lkUrl:        config.LkUrl,
		},
		config.SkipVerifyTLS,
		config.FullAccessHomeservers,
		config.SanityCheckInterval,
	)

	sanityCheckIntervalDisplay := "disabled"
	if config.SanityCheckInterval > 0 {
		sanityCheckIntervalDisplay = config.SanityCheckInterval.String()
	}
	slog.Info("Starting service",
		"LIVEKIT_URL", config.LkUrl,
		"LIVEKIT_JWT_BIND", config.LkJwtBind,
		"LIVEKIT_FULL_ACCESS_HOMESERVERS", config.FullAccessHomeservers,
		"SkipVerifyTLS", config.SkipVerifyTLS,
		"SanityCheckInterval", sanityCheckIntervalDisplay,
	)

	log.Fatal(http.ListenAndServe(config.LkJwtBind, handler.prepareMux()))
}
