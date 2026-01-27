// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"os"
	"reflect"
	"testing"
)

func TestReadKeySecret(t *testing.T) {
	testCases := []struct {
		name           string
		env            map[string]string
		expectedKey    string
		expectedSecret string
		err            bool
	}{
		{
			name: "Read from env",
			env: map[string]string{
				"LIVEKIT_KEY":    "from_env_pheethiewixohp9eecheeGhuayeeph4l",
				"LIVEKIT_SECRET": "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
			},
			expectedKey:    "from_env_pheethiewixohp9eecheeGhuayeeph4l",
			expectedSecret: "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
			err:            false,
		},
		{
			name: "Read from livekit keysecret",
			env: map[string]string{
				"LIVEKIT_KEY_FILE": "./tests/keysecret.yaml",
			},
			expectedKey:    "keysecret_iethuB2LeLiNuishiaKeephei9jaatio",
			expectedSecret: "keysecret_xefaingo4oos6ohla9phiMieBu3ohJi2",
		},
		{
			name: "Read from file",
			env: map[string]string{
				"LIVEKIT_KEY_FROM_FILE":    "./tests/key",
				"LIVEKIT_SECRET_FROM_FILE": "./tests/secret",
			},
			expectedKey:    "from_file_oquusheiheiw4Iegah8te3Vienguus5a",
			expectedSecret: "from_file_vohmahH3eeyieghohSh3kee8feuPhaim",
		},
		{
			name: "Read from file key only",
			env: map[string]string{
				"LIVEKIT_KEY_FROM_FILE": "./tests/key",
				"LIVEKIT_SECRET":        "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
			},
			expectedKey:    "from_file_oquusheiheiw4Iegah8te3Vienguus5a",
			expectedSecret: "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
		},
		{
			name: "Read from file secret only",
			env: map[string]string{
				"LIVEKIT_SECRET_FROM_FILE": "./tests/secret",
				"LIVEKIT_KEY":              "from_env_qui8aiTopiekiechah9oocbeimeew2O",
			},
			expectedKey:    "from_env_qui8aiTopiekiechah9oocbeimeew2O",
			expectedSecret: "from_file_vohmahH3eeyieghohSh3kee8feuPhaim",
		},
		{
			name:           "Empty if secret no env",
			env:            map[string]string{},
			expectedKey:    "",
			expectedSecret: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				if err := os.Setenv(k, v); err != nil {
					t.Errorf("Failed to set environment variable %s: %v", k, err)
				}
			}

			key, secret := readKeySecret()
			if secret != tc.expectedSecret || key != tc.expectedKey {
				t.Errorf("Expected secret and key to be %s and %s but got %s and %s",
					tc.expectedSecret,
					tc.expectedKey,
					secret,
					key)
			}
			for k := range tc.env {
				if err := os.Unsetenv(k); err != nil {
					t.Errorf("Failed to unset environment variable %s: %v", k, err)
				}
			}
		})
	}
}

func TestParseConfig(t *testing.T) {
	testCases := []struct {
		name       string
		env        map[string]string
		wantConfig *Config
		wantErrMsg string
	}{
		{
			name: "Minimal valid config",
			env: map[string]string{
				"LIVEKIT_KEY":    "test_key",
				"LIVEKIT_SECRET": "test_secret",
				"LIVEKIT_URL":    "wss://test.livekit.cloud",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         false,
				FullAccessHomeservers: []string{"*"},
				LkJwtBind:             ":8080",
			},
		},
		{
			name: "Full config with all options",
			env: map[string]string{
				"LIVEKIT_KEY":                      "test_key",
				"LIVEKIT_SECRET":                   "test_secret",
				"LIVEKIT_URL":                      "wss://test.livekit.cloud",
				"LIVEKIT_FULL_ACCESS_HOMESERVERS":  "example.com, test.com",
				"LIVEKIT_JWT_BIND":                 ":9090",
				"LIVEKIT_INSECURE_SKIP_VERIFY_TLS": "YES_I_KNOW_WHAT_I_AM_DOING",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         true,
				FullAccessHomeservers: []string{"example.com", "test.com"},
				LkJwtBind:             ":9090",
			},
		},
		{
			name: "Legacy port configuration",
			env: map[string]string{
				"LIVEKIT_KEY":      "test_key",
				"LIVEKIT_SECRET":   "test_secret",
				"LIVEKIT_URL":      "wss://test.livekit.cloud",
				"LIVEKIT_JWT_PORT": "9090",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         false,
				FullAccessHomeservers: []string{"*"},
				LkJwtBind:             ":9090",
			},
		},
		{
			name: "Legacy full-access homeservers configuration",
			env: map[string]string{
				"LIVEKIT_KEY":               "test_key",
				"LIVEKIT_SECRET":            "test_secret",
				"LIVEKIT_URL":               "wss://test.livekit.cloud",
				"LIVEKIT_LOCAL_HOMESERVERS": "legacy.com",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         false,
				FullAccessHomeservers: []string{"legacy.com"},
				LkJwtBind:             ":8080",
			},
		},
		{
			name: "Missing required config",
			env: map[string]string{
				"LIVEKIT_KEY": "test_key",
			},
			wantErrMsg: "LIVEKIT_KEY[_FILE], LIVEKIT_SECRET[_FILE] and LIVEKIT_URL environment variables must be set",
		},
		{
			name: "Conflicting bind configuration",
			env: map[string]string{
				"LIVEKIT_KEY":      "test_key",
				"LIVEKIT_SECRET":   "test_secret",
				"LIVEKIT_URL":      "wss://test.livekit.cloud",
				"LIVEKIT_JWT_BIND": ":9090",
				"LIVEKIT_JWT_PORT": "8080",
			},
			wantErrMsg: "LIVEKIT_JWT_BIND and LIVEKIT_JWT_PORT environment variables MUST NOT be set together",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup: set env variables
			for k, v := range tc.env {
				if err := os.Setenv(k, v); err != nil {
					t.Fatalf("Failed to set environment variable %s: %v", k, err)
				}
			}
			defer func() {
				// Cleanup: reset env variables after test
				for k := range tc.env {
					if err := os.Unsetenv(k); err != nil {
						t.Errorf("Failed to unset environment variable %s: %v", k, err)
					}
				}
			}()

			// parse config from env variables
			got, err := parseConfig()

			// Given error(s), check potential error messages
			if tc.wantErrMsg != "" {
				if err == nil {
					t.Errorf("parseConfig() error = nil, wantErr %q", tc.wantErrMsg)
					return
				}
				if err.Error() != tc.wantErrMsg {
					t.Errorf("parseConfig() error = %q, wantErr %q", err.Error(), tc.wantErrMsg)
				}
				return
			}

			// Given no error, check for unexpected error messages
			if err != nil {
				t.Errorf("parseConfig() unexpected error: %v", err)
				return
			}

			// Compare parsed (got) config with wanted config
			if got.Key != tc.wantConfig.Key {
				t.Errorf("Key = %q, want %q", got.Key, tc.wantConfig.Key)
			}
			if got.Secret != tc.wantConfig.Secret {
				t.Errorf("Secret = %q, want %q", got.Secret, tc.wantConfig.Secret)
			}
			if got.LkUrl != tc.wantConfig.LkUrl {
				t.Errorf("LkUrl = %q, want %q", got.LkUrl, tc.wantConfig.LkUrl)
			}
			if got.SkipVerifyTLS != tc.wantConfig.SkipVerifyTLS {
				t.Errorf("SkipVerifyTLS = %v, want %v", got.SkipVerifyTLS, tc.wantConfig.SkipVerifyTLS)
			}
			if !reflect.DeepEqual(got.FullAccessHomeservers, tc.wantConfig.FullAccessHomeservers) {
				t.Errorf("FullAccessHomeservers = %v, want %v", got.FullAccessHomeservers, tc.wantConfig.FullAccessHomeservers)
			}
			if got.LkJwtBind != tc.wantConfig.LkJwtBind {
				t.Errorf("JwtBind = %q, want %q", got.LkJwtBind, tc.wantConfig.LkJwtBind)
			}
		})
	}
}