// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"maps"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestReadKeySecret(t *testing.T) {
	testCases := []struct {
		name           string
		env            map[string]string
		expectedKey    string
		expectedSecret string
	}{
		{
			name: "Read from env",
			env: map[string]string{
				"LIVEKIT_KEY":    "from_env_pheethiewixohp9eecheeGhuayeeph4l",
				"LIVEKIT_SECRET": "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
			},
			expectedKey:    "from_env_pheethiewixohp9eecheeGhuayeeph4l",
			expectedSecret: "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
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

func TestReadCsApiUrlOverrides(t *testing.T) {
	testCases := []struct {
		name        string
		env         string
		expectedMap map[string]CsApiUrl
		expectedErr bool
	}{
		{
			name:        "Empty",
			env:         "",
			expectedMap: map[string]CsApiUrl{},
			expectedErr: false,
		},
		{
			name:        "DNS name",
			env:         "example.com=https://matrix-client.example.com",
			expectedMap: map[string]CsApiUrl{"example.com": "https://matrix-client.example.com"},
			expectedErr: false,
		},
		{
			name:        "DNS name with port",
			env:         "example.com=https://matrix-client.example.com:1234",
			expectedMap: map[string]CsApiUrl{"example.com": "https://matrix-client.example.com:1234"},
			expectedErr: false,
		},
		{
			name:        "IPv4",
			env:         "192.168.1.100=https://matrix-client.example.com",
			expectedMap: map[string]CsApiUrl{"192.168.1.100": "https://matrix-client.example.com"},
			expectedErr: false,
		},
		{
			name:        "IPv4 with port",
			env:         "192.168.1.100:1234=https://matrix-client.example.com",
			expectedMap: map[string]CsApiUrl{"192.168.1.100:1234": "https://matrix-client.example.com"},
			expectedErr: false,
		},
		{
			name:        "IPv6",
			env:         "[2001:0db8:85a3:0000:0000:8a2e:0370:7334]=https://matrix-client.example.com",
			expectedMap: map[string]CsApiUrl{"[2001:0db8:85a3:0000:0000:8a2e:0370:7334]": "https://matrix-client.example.com"},
			expectedErr: false,
		},
		{
			name:        "IPv6 with port",
			env:         "[2001:0db8:85a3:0000:0000:8a2e:0370:7334]:1234=https://matrix-client.example.com",
			expectedMap: map[string]CsApiUrl{"[2001:0db8:85a3:0000:0000:8a2e:0370:7334]:1234": "https://matrix-client.example.com"},
			expectedErr: false,
		},
		{
			name:        "Invalid value at the start",
			env:         "example.com",
			expectedMap: nil,
			expectedErr: true,
		},
		{
			name:        "Invalid value at the end",
			env:         "example.com=https://matrix-client.example.com,example.com",
			expectedMap: nil,
			expectedErr: true,
		},
		{
			name:        "Two DNS names",
			env:         "example.com=https://matrix-client.example.com,example.org=https://matrix-client.example.org",
			expectedMap: map[string]CsApiUrl{"example.com": "https://matrix-client.example.com", "example.org": "https://matrix-client.example.org"},
			expectedErr: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actualMap, actualErr := readCsApiUrlOverrides(tc.env)
			if !maps.Equal(tc.expectedMap, actualMap) || tc.expectedErr && actualErr == nil {
				t.Errorf("Expected map: %v, error: %v, got map: %v, error: %v",
					tc.expectedMap,
					tc.expectedErr,
					actualMap,
					actualErr)
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
			name: "Minimal valid config (explicit wildcard)",
			env: map[string]string{
				"LIVEKIT_KEY":                     "test_key",
				"LIVEKIT_SECRET":                  "test_secret",
				"LIVEKIT_URL":                     "wss://test.livekit.cloud",
				"LIVEKIT_FULL_ACCESS_HOMESERVERS": "*",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         false,
				FullAccessHomeservers: []string{"*"},
				LkJwtBind:             ":8080",
				CsApiUrlOverrides:     map[string]CsApiUrl{},
			},
		},
		{
			name: "Full config with all options",
			env: map[string]string{
				"LIVEKIT_KEY":                           "test_key",
				"LIVEKIT_SECRET":                        "test_secret",
				"LIVEKIT_URL":                           "wss://test.livekit.cloud",
				"LIVEKIT_FULL_ACCESS_HOMESERVERS":       "example.com, test.com",
				"LIVEKIT_JWT_BIND":                      ":9090",
				"LIVEKIT_INSECURE_SKIP_VERIFY_TLS":      "YES_I_KNOW_WHAT_I_AM_DOING",
				"LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS": "30",
				"LIVEKIT_CS_API_URL_OVERRIDES":          "matrix.com=https://matrix-client.matrix.com",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         true,
				FullAccessHomeservers: []string{"example.com", "test.com"},
				LkJwtBind:             ":9090",
				SanityCheckInterval:   30 * time.Second,
				CsApiUrlOverrides:     map[string]CsApiUrl{"matrix.com": "https://matrix-client.matrix.com"},
			},
		},
		{
			name: "Legacy port configuration",
			env: map[string]string{
				"LIVEKIT_KEY":                     "test_key",
				"LIVEKIT_SECRET":                  "test_secret",
				"LIVEKIT_URL":                     "wss://test.livekit.cloud",
				"LIVEKIT_JWT_PORT":                "9090",
				"LIVEKIT_FULL_ACCESS_HOMESERVERS": "*",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         false,
				FullAccessHomeservers: []string{"*"},
				LkJwtBind:             ":9090",
				CsApiUrlOverrides:     map[string]CsApiUrl{},
			},
		},
		{
			name: "Missing LIVEKIT_FULL_ACCESS_HOMESERVERS",
			env: map[string]string{
				"LIVEKIT_KEY":    "test_key",
				"LIVEKIT_SECRET": "test_secret",
				"LIVEKIT_URL":    "wss://test.livekit.cloud",
			},
			wantErrMsg: "LIVEKIT_FULL_ACCESS_HOMESERVERS environment variable must be set to the homeserver(s) you intend to serve — see README for guidance",
		},
		{
			name: "Missing required config",
			env: map[string]string{
				"LIVEKIT_KEY": "test_key",
			},
			wantErrMsg: "LIVEKIT_KEY* / LIVEKIT_SECRET* and LIVEKIT_URL must be set",
		},
		{
			name: "Conflicting bind configuration",
			env: map[string]string{
				"LIVEKIT_KEY":                     "test_key",
				"LIVEKIT_SECRET":                  "test_secret",
				"LIVEKIT_URL":                     "wss://test.livekit.cloud",
				"LIVEKIT_JWT_BIND":                ":9090",
				"LIVEKIT_JWT_PORT":                "8080",
				"LIVEKIT_FULL_ACCESS_HOMESERVERS": "*",
			},
			wantErrMsg: "LIVEKIT_JWT_BIND and LIVEKIT_JWT_PORT must not be set together",
		},
		{
			name: "Sanity check interval invalid",
			env: map[string]string{
				"LIVEKIT_KEY":                           "test_key",
				"LIVEKIT_SECRET":                        "test_secret",
				"LIVEKIT_URL":                           "wss://test.livekit.cloud",
				"LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS": "not-a-number",
				"LIVEKIT_FULL_ACCESS_HOMESERVERS":       "*",
			},
			wantErrMsg: `LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS must be a non-negative integer, got "not-a-number"`,
		},
		{
			name: "Sanity check interval zero disables",
			env: map[string]string{
				"LIVEKIT_KEY":                           "test_key",
				"LIVEKIT_SECRET":                        "test_secret",
				"LIVEKIT_URL":                           "wss://test.livekit.cloud",
				"LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS": "0",
				"LIVEKIT_FULL_ACCESS_HOMESERVERS":       "*",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				FullAccessHomeservers: []string{"*"},
				LkJwtBind:             ":8080",
				SanityCheckInterval:   0,
				CsApiUrlOverrides:     map[string]CsApiUrl{},
			},
		},
		{
			name: "Sanity check interval negative rejected",
			env: map[string]string{
				"LIVEKIT_KEY":                           "test_key",
				"LIVEKIT_SECRET":                        "test_secret",
				"LIVEKIT_URL":                           "wss://test.livekit.cloud",
				"LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS": "-1",
				"LIVEKIT_FULL_ACCESS_HOMESERVERS":       "*",
			},
			wantErrMsg: `LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS must be a non-negative integer, got "-1"`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				if err := os.Setenv(k, v); err != nil {
					t.Fatalf("Failed to set environment variable %s: %v", k, err)
				}
			}
			defer func() {
				for k := range tc.env {
					if err := os.Unsetenv(k); err != nil {
						t.Errorf("Failed to unset environment variable %s: %v", k, err)
					}
				}
			}()

			got, err := parseConfig()

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

			if err != nil {
				t.Errorf("parseConfig() unexpected error: %v", err)
				return
			}

			if !reflect.DeepEqual(got, tc.wantConfig) {
				t.Errorf("Expected config: %v, got: %v", tc.wantConfig, got)
				return
			}
		})
	}
}
