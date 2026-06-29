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

func TestHandler_Restore_ResumesSavedJob(t *testing.T) {
	// Set up the store with a single saved job.
	room := LiveKitRoomAlias("test-room")
	identity := LiveKitIdentity("@user:example.com")
	key := jobKey{Room: room, Identity: identity}
	store := newNotifyingStore()
	_ = store.saveJob(context.Background(), key, storedJob{
		Params: DelayedEventJobParams{
			DelayId:         "restore-delay-id",
			ServerName:      "example.com",
			DelayTimeout:    30 * time.Second,
			LiveKitRoom:     room,
			LiveKitIdentity: identity,
		},
		RestartedAt: time.Now(),
	})

	// Block the job to be added on phase one so that we can emit our
	// own events for the test.
	originalLookup := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = originalLookup })
	LiveKitParticipantExists = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	// Mock all delayed event requests to succeed.
	originalExec := ExecuteDelayedEventAction
	t.Cleanup(func() { ExecuteDelayedEventAction = originalExec })
	ExecuteDelayedEventAction = func(_ CsApiUrl, _ string, _ DelayEventAction) (int, error) {
		return http.StatusOK, nil
	}

	// Kick off the handler.
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false,
		[]string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]CsApiUrl{},
		store,
	)
	t.Cleanup(handler.Close)

	// Wait for startup recovery to complete.
	select {
	case <-handler.recoveryDone:
		jobs, err := store.allJobs(context.Background())
		if err != nil {
			t.Fatal("could not load jobs from store")
		}
		// Our job should still be in the store.
		if len(jobs) != 1 {
			t.Fatalf("expected 1 job in the store, found %d", len(jobs))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for handler to complete recovery")
	}

	// Simulate a connect and disconnect cycle for the LiveKit identity.
	handler.sfuEventCh <- sfuEventRequest{
		roomAlias: room,
		msg:       SFUMessage{Type: ParticipantConnected, LiveKitIdentity: identity},
	}
	handler.sfuEventCh <- sfuEventRequest{
		roomAlias: room,
		msg:       SFUMessage{Type: ParticipantDisconnectedIntentionally, LiveKitIdentity: identity},
	}

	// Wait for the job to finish and be deleted from the store.
	select {
	case actualKey := <-store.deletedCh:
		if actualKey != key {
			t.Fatalf("expected delete for %v, observed delete for %v", key, actualKey)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for job to be deleted from store")
	}
}

func TestHandler_Restore_PurgesExpiredJobs(t *testing.T) {
	// Set up the store with an expired job.
	room := LiveKitRoomAlias("test-room")
	identity := LiveKitIdentity("@user:example.com")
	key := jobKey{Room: room, Identity: identity}
	store := newInMemoryStore()
	_ = store.saveJob(context.Background(), key, storedJob{
		Params: DelayedEventJobParams{
			DelayId:         "expired-delay-id",
			ServerName:      "example.com",
			DelayTimeout:    1 * time.Second,
			LiveKitRoom:     room,
			LiveKitIdentity: identity,
		},
		RestartedAt: time.Now().Add(-2 * time.Second),
	})

	// Verify that LiveKitParticipantExists is never called.
	originalLookup := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = originalLookup })
	LiveKitParticipantExists = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		t.Fatal("should not call LiveKitParticipantExists")
		return false, ctx.Err()
	}

	// Verify that ExecuteDelayedEventAction is never called.
	originalExec := ExecuteDelayedEventAction
	t.Cleanup(func() { ExecuteDelayedEventAction = originalExec })
	ExecuteDelayedEventAction = func(_ CsApiUrl, _ string, _ DelayEventAction) (int, error) {
		t.Fatal("should not call ExecuteDelayedEventAction")
		return http.StatusOK, nil
	}

	// Kick off the handler.
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false,
		[]string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]CsApiUrl{},
		store,
	)
	t.Cleanup(handler.Close)

	// Wait for startup recovery to complete.
	select {
	case <-handler.recoveryDone:
		jobs, err := store.allJobs(context.Background())
		if err != nil {
			t.Fatal("could not load jobs from store")
		}
		// Our job should have been deleted from the store.
		if len(jobs) != 0 {
			t.Fatalf("expected 0 jobs in the store, found %d", len(jobs))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for handler to complete recovery")
	}
}

func TestHandler_Restore_GracefullyIgnoresStoreErrors(t *testing.T) {
	// Block the job to be added on phase one so that it doesn't go off
	// do anything for the sake of the test.
	originalLookup := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = originalLookup })
	LiveKitParticipantExists = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	// Kick off the handler with a store that always fails.
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false, []string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]CsApiUrl{},
		&failingStore{},
	)
	t.Cleanup(handler.Close)

	// The handler should still accept new jobs.
	err := handler.addDelayedEventJob(DelayedEventJobParams{
		ServerName:      "example.com",
		DelayId:         "new-delay-id",
		DelayTimeout:    10 * time.Second,
		LiveKitRoom:     LiveKitRoomAlias("error-recovery-room"),
		LiveKitIdentity: LiveKitIdentity("@user:example.com:device:member"),
	})
	if err != nil {
		t.Fatalf("addDelayedEventJob failed: %v", err)
	}
}

func TestHandler_Restore_GracefullyIgnoresMissingStore(t *testing.T) {
	// Block the job to be added on phase one so that it doesn't go off
	// do anything for the sake of the test.
	originalLookup := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = originalLookup })
	LiveKitParticipantExists = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	// Kick off the handler without a store.
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false, []string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]CsApiUrl{},
		nil,
	)
	t.Cleanup(handler.Close)

	// The handler should still accept new jobs.
	err := handler.addDelayedEventJob(DelayedEventJobParams{
		ServerName:      "example.com",
		DelayId:         "new-delay-id",
		DelayTimeout:    10 * time.Second,
		LiveKitRoom:     LiveKitRoomAlias("error-recovery-room"),
		LiveKitIdentity: LiveKitIdentity("@user:example.com:device:member"),
	})
	if err != nil {
		t.Fatalf("addDelayedEventJob failed: %v", err)
	}
}

func TestHandler_Restore_SavesNewJobs(t *testing.T) {
	// Set up an empty store.
	store := newNotifyingStore()

	// Block the job to be added on phase one so that we can emit our
	// own events for the test.
	originalLookup := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = originalLookup })
	LiveKitParticipantExists = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	// Mock all delayed event requests to succeed.
	originalExec := ExecuteDelayedEventAction
	t.Cleanup(func() { ExecuteDelayedEventAction = originalExec })
	ExecuteDelayedEventAction = func(_ CsApiUrl, _ string, _ DelayEventAction) (int, error) {
		return http.StatusOK, nil
	}

	// Kick off the handler.
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false,
		[]string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]CsApiUrl{},
		store,
	)
	t.Cleanup(handler.Close)

	// Wait for startup recovery to complete.
	select {
	case <-handler.recoveryDone:
		jobs, err := store.allJobs(context.Background())
		if err != nil {
			t.Fatal("could not load jobs from store")
		}
		// Our job should still be in the store.
		if len(jobs) != 0 {
			t.Fatalf("expected 0 jobs in the store, found %d", len(jobs))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for handler to complete recovery")
	}

	// Add a new job.
	room := LiveKitRoomAlias("test-room")
	identity := LiveKitIdentity("@user:example.com")
	key := jobKey{Room: room, Identity: identity}
	err := handler.addDelayedEventJob(DelayedEventJobParams{
		ServerName:      "example.com",
		DelayId:         "new-delay-id",
		DelayTimeout:    10 * time.Second,
		LiveKitRoom:     room,
		LiveKitIdentity: identity,
	})
	if err != nil {
		t.Fatalf("addDelayedEventJob failed: %v", err)
	}

	// Wait for the job to be saved into the store.
	select {
	case actualKey := <-store.savedCh:
		if actualKey != key {
			t.Fatalf("expected save for %v, observed save for %v", key, actualKey)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for job to be saved into the store")
	}

	// Simulate a connect and disconnect cycle for the LiveKit identity.
	handler.sfuEventCh <- sfuEventRequest{
		roomAlias: room,
		msg:       SFUMessage{Type: ParticipantConnected, LiveKitIdentity: identity},
	}
	handler.sfuEventCh <- sfuEventRequest{
		roomAlias: room,
		msg:       SFUMessage{Type: ParticipantDisconnectedIntentionally, LiveKitIdentity: identity},
	}

	// Wait for the job to finish and be deleted from the store.
	select {
	case actualKey := <-store.deletedCh:
		if actualKey != key {
			t.Fatalf("expected delete for %v, observed delete for %v", key, actualKey)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for job to be deleted from store")
	}
}

// An in-memory store that notifies about job save and deletion.
type notifyingStore struct {
	store
	savedCh   chan jobKey
	deletedCh chan jobKey
}

func newNotifyingStore() *notifyingStore {
	return &notifyingStore{
		store:     newInMemoryStore(),
		savedCh:   make(chan jobKey, 10),
		deletedCh: make(chan jobKey, 10)}
}

func (s *notifyingStore) saveJob(ctx context.Context, key jobKey, job storedJob) error {
	err := s.store.saveJob(ctx, key, job)
	if err == nil {
		s.savedCh <- key
	}
	return err
}

func (s *notifyingStore) deleteJob(ctx context.Context, key jobKey) error {
	err := s.store.deleteJob(ctx, key)
	if err == nil {
		s.deletedCh <- key
	}
	return err
}

// A store that fails on any operation.
type failingStore struct{}

func (s *failingStore) saveJob(_ context.Context, _ jobKey, _ storedJob) error {
	return errors.New("failed")
}
func (s *failingStore) deleteJob(_ context.Context, _ jobKey) error {
	return errors.New("failed")
}

func (s *failingStore) allJobs(_ context.Context) ([]storedJob, error) {
	return nil, errors.New("failed")
}
