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

	"github.com/redis/go-redis/v9"
)

const redisJobsHashKey = "lk-jwt:jobs"

// A store backed by an external Redis instance.
type redisJobStore struct {
	client *redis.Client
}

func newRedisJobStore(redisURL string) (JobStore, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("store: invalid Redis URL %q: %w", redisURL, err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("store: Redis ping failed: %w", err)
	}
	slog.Info("store: connected to Redis", "addr", opts.Addr)
	return &redisJobStore{client: client}, nil
}

func (s *redisJobStore) Save(ctx context.Context, identity LiveKitIdentity, job PersistedJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	return s.client.HSet(ctx, redisJobsHashKey, string(identity), data).Err()
}

func (s *redisJobStore) Delete(ctx context.Context, identity LiveKitIdentity) error {
	return s.client.HDel(ctx, redisJobsHashKey, string(identity)).Err()
}

func (s *redisJobStore) LoadAll(ctx context.Context) ([]PersistedJob, error) {
	fields, err := s.client.HGetAll(ctx, redisJobsHashKey).Result()
	if err != nil {
		return nil, fmt.Errorf("store: failed getting all entries: %w", err)
	}
	jobs := make([]PersistedJob, 0, len(fields))
	for identity, data := range fields {
		var pj PersistedJob
		if err := json.Unmarshal([]byte(data), &pj); err != nil {
			slog.Warn("store: skipping unparseable entry", "identity", identity, "err", err)
			continue
		}
		jobs = append(jobs, pj)
	}
	return jobs, nil
}
