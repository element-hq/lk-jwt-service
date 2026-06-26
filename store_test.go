// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// ── shared test suite ─────────────────────────────────────────────────────────

func runStoreTests(t *testing.T, store JobStore) {
	t.Helper()
	ctx := context.Background()

	identity := LiveKitIdentity("@user:example.com:device-id:member-id")
	params := DelayedEventJobParams{
		DelayId:         "delay-1",
		ServerName:      "example.com",
		DelayTimeout:    30 * time.Second,
		LiveKitRoom:     LiveKitRoomAlias("room-abc"),
		LiveKitIdentity: identity,
	}
	pj := PersistedJob{Params: params, CreatedAt: time.Now().Truncate(time.Millisecond)}

	t.Run("LoadAll_empty", func(t *testing.T) {
		jobs, err := store.LoadAll(ctx)
		if err != nil {
			t.Fatalf("LoadAll on empty store: %v", err)
		}
		if len(jobs) != 0 {
			t.Errorf("expected 0 jobs, got %d", len(jobs))
		}
	})

	t.Run("Save_and_LoadAll", func(t *testing.T) {
		if err := store.Save(ctx, identity, pj); err != nil {
			t.Fatalf("Save: %v", err)
		}
		t.Cleanup(func() { _ = store.Delete(ctx, identity) })

		jobs, err := store.LoadAll(ctx)
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("expected 1 job, got %d", len(jobs))
		}
		got := jobs[0]
		if got.Params.DelayId != pj.Params.DelayId {
			t.Errorf("DelayId = %q, want %q", got.Params.DelayId, pj.Params.DelayId)
		}
		if got.Params.ServerName != pj.Params.ServerName {
			t.Errorf("CsApiUrl = %q, want %q", got.Params.ServerName, pj.Params.ServerName)
		}
		if got.Params.DelayTimeout != pj.Params.DelayTimeout {
			t.Errorf("DelayTimeout = %q, want %q", got.Params.DelayTimeout, pj.Params.DelayTimeout)
		}
		if got.Params.LiveKitRoom != pj.Params.LiveKitRoom {
			t.Errorf("LiveKitRoom = %q, want %q", got.Params.LiveKitRoom, pj.Params.LiveKitRoom)
		}
		if got.Params.DelayTimeout != pj.Params.DelayTimeout {
			t.Errorf("DelayTimeout = %v, want %v", got.Params.DelayTimeout, pj.Params.DelayTimeout)
		}
		if !got.CreatedAt.Equal(pj.CreatedAt) {
			t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, pj.CreatedAt)
		}
	})

	t.Run("Save_overwrites", func(t *testing.T) {
		if err := store.Save(ctx, identity, pj); err != nil {
			t.Fatalf("Save first: %v", err)
		}
		t.Cleanup(func() { _ = store.Delete(ctx, identity) })

		updated := pj
		updated.Params.DelayId = "delay-updated"
		if err := store.Save(ctx, identity, updated); err != nil {
			t.Fatalf("Save second: %v", err)
		}

		jobs, err := store.LoadAll(ctx)
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("expected 1 job after overwrite, got %d", len(jobs))
		}
		if jobs[0].Params.DelayId != "delay-updated" {
			t.Errorf("overwrite did not take effect: got DelayId=%q", jobs[0].Params.DelayId)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		identity2 := LiveKitIdentity("@other:example.com:device-id:member-id")
		pj2 := pj
		pj2.Params.LiveKitIdentity = identity2

		if err := store.Save(ctx, identity, pj); err != nil {
			t.Fatalf("Save first: %v", err)
		}
		if err := store.Save(ctx, identity2, pj2); err != nil {
			t.Fatalf("Save second: %v", err)
		}
		t.Cleanup(func() {
			_ = store.Delete(ctx, identity)
			_ = store.Delete(ctx, identity2)
		})

		if err := store.Delete(ctx, identity); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		jobs, err := store.LoadAll(ctx)
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("expected 1 job after delete, got %d", len(jobs))
		}
		if jobs[0].Params.LiveKitIdentity != identity2 {
			t.Errorf("wrong job remaining: got identity=%q", jobs[0].Params.LiveKitIdentity)
		}
	})

	t.Run("Delete_missing_is_not_error", func(t *testing.T) {
		if err := store.Delete(ctx, LiveKitIdentity("@nonexistent:example.com")); err != nil {
			t.Errorf("Delete of missing key should not error, got: %v", err)
		}
	})
}

// ── memory store ──────────────────────────────────────────────────────────────

func TestMemoryJobStore(t *testing.T) {
	runStoreTests(t, newMemoryJobStore())
}

// ── Redis store ───────────────────────────────────────────────────────────────

func newMiniredisStore(t *testing.T) JobStore {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &redisJobStore{client: client}
}

func TestRedisJobStore(t *testing.T) {
	runStoreTests(t, newMiniredisStore(t))
}

func TestRedisJobStore_SkipsUnparseableEntry(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := &redisJobStore{client: client}

	ctx := context.Background()
	// Write a corrupt JSON entry directly via the Redis client.
	if err := client.HSet(ctx, redisJobsHashKey, "@bad:identity", "not valid json").Err(); err != nil {
		t.Fatalf("HSet: %v", err)
	}
	// Write a valid entry alongside it.
	identity := LiveKitIdentity("@good:example.com")
	pj := PersistedJob{
		Params:    DelayedEventJobParams{DelayId: "id", LiveKitIdentity: identity},
		CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, identity, pj); err != nil {
		t.Fatalf("Save: %v", err)
	}

	jobs, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll should not fail on corrupt entries: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 valid job, got %d", len(jobs))
	}
	if jobs[0].Params.DelayId != pj.Params.DelayId {
		t.Fatalf("DelayId = %q, want %q", jobs[0].Params.DelayId, pj.Params.DelayId)
	}
}
