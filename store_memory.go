// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"maps"
	"slices"
)

// A non-persistent, in-memory JobStore.
type memoryJobStore struct {
	jobs map[LiveKitIdentity]PersistedJob
}

func newMemoryJobStore() JobStore {
	return &memoryJobStore{jobs: make(map[LiveKitIdentity]PersistedJob)}
}

func (s *memoryJobStore) Save(_ context.Context, identity LiveKitIdentity, job PersistedJob) error {
	s.jobs[identity] = job
	return nil
}

func (s *memoryJobStore) Delete(_ context.Context, identity LiveKitIdentity) error {
	delete(s.jobs, identity)
	return nil
}

func (s *memoryJobStore) LoadAll(_ context.Context) ([]PersistedJob, error) {
	return slices.Collect(maps.Values(s.jobs)), nil
}
