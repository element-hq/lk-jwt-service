// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func runStoreTests(t *testing.T, newStore func() store) {
	t.Helper()
	ctx := context.Background()

	identity := LiveKitIdentity("@user:example.com")
	params := DelayedEventJobParams{
		DelayId:         "id",
		ServerName:      "example.com",
		DelayTimeout:    30 * time.Second,
		LiveKitRoom:     LiveKitRoomAlias("!room:example.com"),
		LiveKitIdentity: identity,
	}
	job := storedJob{Params: params, CreatedAt: time.Now().Truncate(time.Millisecond)}

	t.Run("TestLoadAllJobsOnEmptyStore", func(t *testing.T) {
		store := newStore()

		// Load all jobs and check that the store is empty.
		jobs, err := store.allJobs(ctx)
		if err != nil {
			t.Fatalf("getting all jobs failed: %v", err)
		}
		if len(jobs) != 0 {
			t.Errorf("expected 0 jobs, got %d", len(jobs))
		}
	})

	t.Run("TestSaveJobOnEmptyStore", func(t *testing.T) {
		store := newStore()

		// Save a job.
		if err := store.saveJob(ctx, identity, job); err != nil {
			t.Fatalf("Saving job failed: %v", err)
		}

		// Load all jobs and check that we get the saved job.
		jobs, err := store.allJobs(ctx)
		if err != nil {
			t.Fatalf("getting all jobs failed: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("expected 1 job, got %d", len(jobs))
		}
		got := jobs[0]
		if !reflect.DeepEqual(job, got) {
			t.Fatalf("expected %v, got %v", job, got)
		}
	})

	t.Run("TestSaveJobOverwrites", func(t *testing.T) {
		store := newStore()

		// Save a job.
		if err := store.saveJob(ctx, identity, job); err != nil {
			t.Fatalf("saving first job failed: %v", err)
		}

		// Save an updated job for the same identity.
		updated := job
		updated.Params.DelayId = "delay-updated"
		if err := store.saveJob(ctx, identity, updated); err != nil {
			t.Fatalf("saving second job failed second: %v", err)
		}

		// Load all jobs and check that we get the updated job.
		jobs, err := store.allJobs(ctx)
		if err != nil {
			t.Fatalf("getting all jobs failed: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("expected 1 job after overwrite, got %d", len(jobs))
		}
		got := jobs[0]
		if !reflect.DeepEqual(updated, got) {
			t.Fatalf("expected %v, got %v", updated, got)
		}
	})

	t.Run("TestDeleteJob", func(t *testing.T) {
		store := newStore()

		// Save a job.
		if err := store.saveJob(ctx, identity, job); err != nil {
			t.Fatalf("saving first job failed: %v", err)
		}

		// Save another job.
		identity2 := LiveKitIdentity("@other:example.com:device-id:member-id")
		job2 := job
		job2.Params.LiveKitIdentity = identity2
		if err := store.saveJob(ctx, identity2, job2); err != nil {
			t.Fatalf("saving second job failed: %v", err)
		}

		// Delete the first job.
		if err := store.deleteJob(ctx, identity); err != nil {
			t.Fatalf("deleting first job failed: %v", err)
		}

		// Load all jobs and check that we still get the second job.
		jobs, err := store.allJobs(ctx)
		if err != nil {
			t.Fatalf("getting all jobs failed: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("expected 1 job after delete, got %d", len(jobs))
		}
		got := jobs[0]
		if !reflect.DeepEqual(job2, got) {
			t.Fatalf("expected %v, got %v", job2, got)
		}
	})

	t.Run("TestDeleteMissingJob", func(t *testing.T) {
		store := newStore()

		// Delete a job that doesn't exist in the store.
		if err := store.deleteJob(ctx, LiveKitIdentity("@nonexistent:example.com")); err != nil {
			t.Errorf("deleting missing job failed: %v", err)
		}
	})
}

// In-memory store tests

func testInMemoryStore(t *testing.T) {
	runStoreTests(t, newInMemoryStore)
}

// Redis store tests

func newMiniRedisStore(t *testing.T) (store, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &redisStore{client: client}, client
}

func testRedisStore(t *testing.T) {
	runStoreTests(t, func() store {
		store, _ := newMiniRedisStore(t)
		return store
	})
}

func testRedisStoreSkipsUnparseableEntry(t *testing.T) {
	store, client := newMiniRedisStore(t)

	// Save a corrupt JSON entry directly via the Redis client.
	ctx := context.Background()
	if err := client.HSet(ctx, redisJobsHashKey, "@bad:identity", "not valid json").Err(); err != nil {
		t.Fatalf("writing into store failed: %v", err)
	}

	// Save a valid entry alongside it.
	identity := LiveKitIdentity("@good:example.com")
	job := storedJob{
		Params:    DelayedEventJobParams{DelayId: "id", LiveKitIdentity: identity},
		CreatedAt: time.Now(),
	}
	if err := store.saveJob(ctx, identity, job); err != nil {
		t.Fatalf("saving job failed: %v", err)
	}

	// Load all jobs and check that we only get the valid entry.
	jobs, err := store.allJobs(ctx)
	if err != nil {
		t.Fatalf("getting all jobs failed: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 valid job, got %d", len(jobs))
	}
	got := jobs[0]
	if !reflect.DeepEqual(job, got) {
		t.Fatalf("expected %v, got %v", job, got)
	}
}
