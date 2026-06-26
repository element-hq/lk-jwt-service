// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
)

// A stored job.
//
// The fields on this struct are exported so that we can serialise
// them, e.g. to JSON.
type storedJob struct {
	Params      DelayedEventJobParams
	RestartedAt time.Time
}

// Interface for storage backends.
type store interface {
	// Add a job to the store, overwriting any existing entry for the same identity.
	//
	// The caller is responsible for ensuring thread-safety when calling this and
	// other store methods.
	saveJob(ctx context.Context, identity LiveKitIdentity, job storedJob) error

	// Remove the entry for the given identity from the store. A missing entry is not an error.
	//
	// The caller is responsible for ensuring thread-safety when calling this and
	// other store methods.
	deleteJob(ctx context.Context, identity LiveKitIdentity) error

	// Retrieve all jobs in the store.
	//
	// The caller is responsible for ensuring thread-safety when calling this and
	// other store methods.
	allJobs(ctx context.Context) ([]storedJob, error)
}

// An in-memory storage backend without persistency.
type inMemoryStore struct {
	jobs map[LiveKitIdentity]storedJob
}

func newInMemoryStore() store {
	store := &inMemoryStore{jobs: make(map[LiveKitIdentity]storedJob)}
	slog.Info("store: created new in-memory store")
	return store
}

func (s *inMemoryStore) saveJob(_ context.Context, identity LiveKitIdentity, job storedJob) error {
	s.jobs[identity] = job
	return nil
}

func (s *inMemoryStore) deleteJob(_ context.Context, identity LiveKitIdentity) error {
	delete(s.jobs, identity)
	return nil
}

func (s *inMemoryStore) allJobs(_ context.Context) ([]storedJob, error) {
	return slices.Collect(maps.Values(s.jobs)), nil
}

// A store backend using an external Redis instance.
type redisStore struct {
	client *redis.Client
}

const redisJobsHashKey = "lk-jwt:jobs"

func newRedisStore(redisURL string) (store, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("store: invalid Redis URL %q: %w", redisURL, err)
	}

	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("store: Redis ping failed: %w", err)
	}

	slog.Info("store: connected to Redis", "addr", opts.Addr)
	return &redisStore{client: client}, nil
}

func (s *redisStore) saveJob(ctx context.Context, identity LiveKitIdentity, job storedJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("store: failed marshalling job: %w", err)
	}
	return s.client.HSet(ctx, redisJobsHashKey, string(identity), data).Err()
}

func (s *redisStore) deleteJob(ctx context.Context, identity LiveKitIdentity) error {
	return s.client.HDel(ctx, redisJobsHashKey, string(identity)).Err()
}

func (s *redisStore) allJobs(ctx context.Context) ([]storedJob, error) {
	fields, err := s.client.HGetAll(ctx, redisJobsHashKey).Result()
	if err != nil {
		return nil, fmt.Errorf("store: failed getting all entries: %w", err)
	}

	jobs := make([]storedJob, 0, len(fields))

	for identity, data := range fields {
		var job storedJob
		if err := json.Unmarshal([]byte(data), &job); err != nil {
			slog.Warn("store: skipping unparseable entry", "identity", identity, "err", err)
			continue
		}
		jobs = append(jobs, job)
	}

	return jobs, nil
}
