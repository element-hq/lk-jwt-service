// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"time"
)

// PersistedJob holds the data required to reconstruct a delayed event job.
type PersistedJob struct {
	Params    DelayedEventJobParams
	CreatedAt time.Time
}

// JobStore persists delayed event jobs.
//
// The store is keyed by LiveKitIdentity, which encodes Matrix user ID, device
// ID, and MatrixRTC member ID and is therefore globally unique per connection
// attempt.
//
// All methods are called only from Handler.loop() and need not be goroutine-safe.
type JobStore interface {
	// Persist a job, overwriting any existing entry for the same identity.
	Save(ctx context.Context, identity LiveKitIdentity, job PersistedJob) error
	// Removes the entry for the given identity. A missing entry is not an error.
	Delete(ctx context.Context, identity LiveKitIdentity) error
	// Retrieve all persisted jobs.
	LoadAll(ctx context.Context) ([]PersistedJob, error)
}
