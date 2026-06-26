// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestHandler_Restore_ResumesLiveJobs(t *testing.T) {
	originalLookup := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = originalLookup })
	LiveKitParticipantExists = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	originalExec := ExecuteDelayedEventAction
	t.Cleanup(func() { ExecuteDelayedEventAction = originalExec })
	ExecuteDelayedEventAction = func(_ CsApiUrl, _ string, _ DelayEventAction) (int, error) {
		return http.StatusOK, nil
	}

	identity := LiveKitIdentity("@restore-user:example.com:device:member")
	room := LiveKitRoomAlias("restore-test-room")

	store := newInMemoryStore()
	_ = store.saveJob(context.Background(), identity, storedJob{
		Params: DelayedEventJobParams{
			DelayId:         "restore-delay-id",
			ServerName:      "example.com",
			DelayTimeout:    30 * time.Second,
			LiveKitRoom:     room,
			LiveKitIdentity: identity,
		},
		CreatedAt: time.Now(),
	})

	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false, []string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]CsApiUrl{},
		store,
	)
	t.Cleanup(handler.Close)

	// Give loop() time to run startup recovery.
	time.Sleep(50 * time.Millisecond)

	// The restored job must be routable — send it a connect event followed by
	// a disconnect so it completes its full lifecycle without deadlocking.
	handler.sfuEventCh <- sfuEventRequest{
		roomAlias: room,
		msg:       SFUMessage{Type: ParticipantConnected, LiveKitIdentity: identity},
	}
	time.Sleep(30 * time.Millisecond)
	handler.sfuEventCh <- sfuEventRequest{
		roomAlias: room,
		msg:       SFUMessage{Type: ParticipantDisconnectedIntentionally, LiveKitIdentity: identity},
	}

	// Wait for the job to finish and be cleaned up (delete from store).
	time.Sleep(200 * time.Millisecond)

	// After the job completes, the store entry should have been deleted.
	jobs, err := store.allJobs(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected store to be empty after job completion, got %d entries", len(jobs))
	}
}

func TestHandler_Restore_SkipsExpiredJobs(t *testing.T) {
	identity := LiveKitIdentity("@expired:example.com:device:member")
	room := LiveKitRoomAlias("expired-room")

	store := newInMemoryStore()
	_ = store.saveJob(context.Background(), identity, storedJob{
		Params: DelayedEventJobParams{
			DelayId:         "expired-delay-id",
			ServerName:      "example.com",
			DelayTimeout:    1 * time.Second,
			LiveKitRoom:     room,
			LiveKitIdentity: identity,
		},
		// CreatedAt far enough in the past that DelayTimeout has elapsed.
		CreatedAt: time.Now().Add(-2 * time.Second),
	})

	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false, []string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]CsApiUrl{},
		store,
	)
	t.Cleanup(handler.Close)

	// Give loop() time to run startup recovery and clean up the expired entry.
	time.Sleep(50 * time.Millisecond)

	// Expired job must not be routable.
	handler.sfuEventCh <- sfuEventRequest{
		roomAlias: room,
		msg:       SFUMessage{Type: ParticipantConnected, LiveKitIdentity: identity},
	}
	time.Sleep(30 * time.Millisecond)

	// The store entry must have been deleted during recovery.
	jobs, err := store.allJobs(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected expired job to be deleted from store, got %d entries", len(jobs))
	}
}

func TestHandler_Restore_StoreError(t *testing.T) {
	originalLookup := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = originalLookup })
	LiveKitParticipantExists = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false, []string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]CsApiUrl{},
		&failingLoadStore{},
	)
	t.Cleanup(handler.Close)

	// Handler must still accept new jobs despite the startup error.
	err := handler.addDelayedEventJob(DelayedEventJobParams{
		ServerName:      "example.com",
		DelayId:         "new-delay-id",
		DelayTimeout:    10 * time.Second,
		LiveKitRoom:     LiveKitRoomAlias("error-recovery-room"),
		LiveKitIdentity: LiveKitIdentity("@user:example.com:device:member"),
	})
	if err != nil {
		t.Fatalf("addDelayedEventJob after store error: %v", err)
	}
}

func TestHandler_SaveError_FailsRequest(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false, []string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]CsApiUrl{},
		&failingSaveStore{},
	)
	t.Cleanup(handler.Close)

	err := handler.addDelayedEventJob(DelayedEventJobParams{
		ServerName:      "example.com",
		DelayId:         "save-fail-id",
		DelayTimeout:    10 * time.Second,
		LiveKitRoom:     LiveKitRoomAlias("save-fail-room"),
		LiveKitIdentity: LiveKitIdentity("@user:example.com:device:member"),
	})
	if !errors.Is(err, errStorePersistFailed) {
		t.Fatalf("expected errStorePersistFailed, got: %v", err)
	}

	mErr := matrixErrorForAddJob(err)
	if mErr.Status != http.StatusServiceUnavailable || mErr.ErrCode != "M_UNKNOWN" {
		t.Errorf("expected 503 M_UNKNOWN, got %d %s", mErr.Status, mErr.ErrCode)
	}
}

// ── stub stores ───────────────────────────────────────────────────────────────

type failingLoadStore struct{}

func (s *failingLoadStore) saveJob(_ context.Context, _ LiveKitIdentity, _ storedJob) error {
	return nil
}
func (s *failingLoadStore) deleteJob(_ context.Context, _ LiveKitIdentity) error { return nil }
func (s *failingLoadStore) allJobs(_ context.Context) ([]storedJob, error) {
	return nil, errors.New("simulated LoadAll failure")
}

type failingSaveStore struct{}

func (s *failingSaveStore) saveJob(_ context.Context, _ LiveKitIdentity, _ storedJob) error {
	return errors.New("simulated Save failure")
}
func (s *failingSaveStore) deleteJob(_ context.Context, _ LiveKitIdentity) error { return nil }
func (s *failingSaveStore) allJobs(_ context.Context) ([]storedJob, error) {
	return nil, nil
}
