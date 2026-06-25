// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	lkJwtBind := os.Getenv("LIVEKIT_JWT_BIND")
	if lkJwtBind == "" {
		lkJwtBind = "8080"
	}

	resp, err := http.Get(fmt.Sprintf("http://localhost:%s/healthz", lkJwtBind))
	if err != nil {
		fmt.Println("Connection error:", err)
		os.Exit(1)
	}

	if resp.StatusCode != 200 {
		fmt.Println("Healthcheck failed with status code", resp.StatusCode)
		os.Exit(1)
	}
}
