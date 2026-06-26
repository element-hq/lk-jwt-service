// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// config.go: environment-driven configuration parsing for the service —
// LiveKit credentials, JWT bind address, full-access homeservers, and the
// optional sanity-check interval.

package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Key                   string
	Secret                string
	LkUrl                 string
	SkipVerifyTLS         bool
	FullAccessHomeservers []string
	LkJwtBind             string
	// SanityCheckInterval is the period at which the job worker re-checks
	// whether connected participants are still present on the SFU. Zero (the
	// default) disables the sanity check entirely.
	// Configure via LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS (unit: seconds).
	SanityCheckInterval time.Duration
	// Map of URLs for the Client-Server API keyed by server name. These will
	// be preferred over .well-known resolution for the contained server names.
	CsApiUrlOverrides map[string]CsApiUrl
	// Connection URL for the Redis job store. When empty, the service falls
	// back to a non-persistent in-memory store.
	// Configure via REDIS_URL (e.g. redis://localhost:6379).
	RedisURL string
}

func readKeySecret() (string, string) {
	key := os.Getenv("LIVEKIT_KEY")
	secret := os.Getenv("LIVEKIT_SECRET")
	keyPath := os.Getenv("LIVEKIT_KEY_FROM_FILE")
	secretPath := os.Getenv("LIVEKIT_SECRET_FROM_FILE")
	keySecretPath := os.Getenv("LIVEKIT_KEY_FILE")

	if keySecretPath != "" {
		keySecretBytes, err := os.ReadFile(keySecretPath)
		if err != nil {
			log.Fatal(err)
		}
		parts := strings.Split(string(keySecretBytes), ":")
		if len(parts) != 2 {
			log.Fatalf("invalid key secret file format!")
		}
		slog.Info("Using LiveKit API key and secret from LIVEKIT_KEY_FILE", "keySecretPath", keySecretPath)
		key = parts[0]
		secret = parts[1]
	} else {
		if keyPath != "" {
			keyBytes, err := os.ReadFile(keyPath)
			if err != nil {
				log.Fatal(err)
			}
			slog.Info("Using LiveKit API key from LIVEKIT_KEY_FROM_FILE", "keyPath", keyPath)
			key = string(keyBytes)
		} else {
			slog.Info("Using LiveKit API key from LIVEKIT_KEY")
		}
		if secretPath != "" {
			secretBytes, err := os.ReadFile(secretPath)
			if err != nil {
				log.Fatal(err)
			}
			slog.Info("Using LiveKit API secret from LIVEKIT_SECRET_FROM_FILE", "secretPath", secretPath)
			secret = string(secretBytes)
		} else {
			slog.Info("Using LiveKit API secret from LIVEKIT_SECRET")
		}
	}

	return strings.Trim(key, " \r\n"), strings.Trim(secret, " \r\n")
}

func readCsApiUrlOverrides(raw string) (map[string]CsApiUrl, error) {
	m := map[string]CsApiUrl{}
	if raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			server, url, ok := strings.Cut(entry, "=")
			if !ok {
				return nil, fmt.Errorf("invalid entry %q, expected server_name=url", entry)
			}
			server = strings.TrimSpace(server)
			url = strings.TrimSpace(url)
			if server == "" || url == "" {
				return nil, fmt.Errorf("invalid entry %q, expected server_name=url", entry)
			}
			m[server] = CsApiUrl(url)
		}
	}
	return m, nil
}

func parseConfig() (*Config, error) {
	skipVerifyTLS := os.Getenv("LIVEKIT_INSECURE_SKIP_VERIFY_TLS") == "YES_I_KNOW_WHAT_I_AM_DOING"
	if skipVerifyTLS {
		slog.Warn("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
		slog.Warn("!!! WARNING !!!  LIVEKIT_INSECURE_SKIP_VERIFY_TLS        !!! WARNING !!!")
		slog.Warn("!!! WARNING !!!  Allow to skip invalid TLS certificates  !!! WARNING !!!")
		slog.Warn("!!! WARNING !!!  Use only for testing or debugging       !!! WARNING !!!")
		slog.Warn("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n")
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	key, secret := readKeySecret()
	lkUrl := os.Getenv("LIVEKIT_URL")

	if key == "" || secret == "" || lkUrl == "" {
		return nil, fmt.Errorf("LIVEKIT_KEY* / LIVEKIT_SECRET* and LIVEKIT_URL must be set")
	}

	fullAccessHomeservers := os.Getenv("LIVEKIT_FULL_ACCESS_HOMESERVERS")
	if len(fullAccessHomeservers) == 0 {
		return nil, fmt.Errorf("LIVEKIT_FULL_ACCESS_HOMESERVERS environment variable must be set to the homeserver(s) you intend to serve — see README for guidance")
	}

	lkJwtBind := os.Getenv("LIVEKIT_JWT_BIND")
	lkJwtPort := os.Getenv("LIVEKIT_JWT_PORT")
	if lkJwtBind == "" {
		if lkJwtPort == "" {
			lkJwtPort = "8080"
		} else {
			slog.Warn("!!! LIVEKIT_JWT_PORT is deprecated, use LIVEKIT_JWT_BIND !!!")
		}
		lkJwtBind = fmt.Sprintf(":%s", lkJwtPort)
	} else if lkJwtPort != "" {
		return nil, fmt.Errorf("LIVEKIT_JWT_BIND and LIVEKIT_JWT_PORT must not be set together")
	}

	var sanityCheckInterval time.Duration
	if s := os.Getenv("LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS"); s != "" {
		secs, err := strconv.Atoi(s)
		if err != nil || secs < 0 {
			return nil, fmt.Errorf("LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS must be a non-negative integer, got %q", s)
		}
		sanityCheckInterval = time.Duration(secs) * time.Second
	}

	csApiUrlOverrides, err := readCsApiUrlOverrides(os.Getenv("LIVEKIT_CS_API_URL_OVERRIDES"))
	if err != nil {
		return nil, fmt.Errorf("failed parsing LIVEKIT_CS_API_URL_OVERRIDES: %w", err)
	}

	return &Config{
		Key:                   key,
		Secret:                secret,
		LkUrl:                 lkUrl,
		SkipVerifyTLS:         skipVerifyTLS,
		FullAccessHomeservers: strings.Fields(strings.ReplaceAll(fullAccessHomeservers, ",", " ")),
		LkJwtBind:             lkJwtBind,
		SanityCheckInterval:   sanityCheckInterval,
		CsApiUrlOverrides:     csApiUrlOverrides,
		RedisURL:              os.Getenv("REDIS_URL"),
	}, nil
}
