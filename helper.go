// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type UniqueID string

func NewUniqueID() UniqueID {
	// 8 bytes for nano-timestamp + 8 bytes for randomness = 16 bytes (128 bits)
	b := make([]byte, 16)

	// 1. 64-bit Microsecond Timestamp
	// Big-Endian ensures higher time units come first, making it sortable.
	binary.BigEndian.PutUint64(b[0:8], uint64(time.Now().UnixMicro()))

	// 2. 64-bit Randomness (Entropy)
	// Provides 18 quintillion possibilities per microsecond to prevent collisions.
	if _, err := rand.Read(b[8:16]); err != nil {
		panic(err)
	}

	// Because Base32Hex uses an alphabet that is naturally ordered in the
	// ASCII/Unicode table (0-9 then A-V), the string comparison results will match
	// the chronological order of your original timestamp.
	return UniqueID(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

var exchangeOpenIdUserInfo = func(
	ctx context.Context, token OpenIDTokenType, skipVerifyTLS bool,
) (*fclient.UserInfo, error) {
	if token.AccessToken == "" || token.MatrixServerName == "" {
		return nil, errors.New("missing parameters in openid token")
	}

	client := fclient.NewClient(fclient.WithWellKnownSRVLookups(true), fclient.WithSkipVerify(skipVerifyTLS))

	// validate the openid token by getting the user's ID
	userinfo, err := client.LookupUserInfo(
		ctx, spec.ServerName(token.MatrixServerName), token.AccessToken,
	)
	if err != nil {
		slog.Error("OpenIDUserInfo: Failed to look up user info", "err", err)
		return nil, fmt.Errorf("failed to look up user info: %w", err)
	}
	return &userinfo, nil
}

var unpaddedBase64 = base64.StdEncoding.WithPadding(base64.NoPadding)

// marshalStrings marshals a slice of strings to JSON.  json.Marshal of
// []string is total — string has no MarshalJSON hook, no cycles are possible,
// no unsupported types can appear — so the error return is discarded.
func marshalStrings(ss []string) []byte {
	b, _ := json.Marshal(ss)
	return b
}

// RoomClient defines the interface for room operations
type RoomClient interface {
	CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error)
	GetParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.ParticipantInfo, error)
}

// mockable lksdk.NewRoomServiceClient for testing
var newRoomServiceClient = func(url, key, secret string) RoomClient {
	return lksdk.NewRoomServiceClient(url, key, secret)
}

func CreateLiveKitRoomAlias(matrixRoom string, matrixRtcSlot string) LiveKitRoomAlias {
	// Create a deterministic LiveKit room alias based on Matrix room ID and slot ID
	// to ensure uniqueness and avoid collisions.
	hash := sha256.Sum256(marshalStrings([]string{matrixRoom, matrixRtcSlot}))
	return LiveKitRoomAlias(unpaddedBase64.EncodeToString(hash[:]))
}

func CreateLiveKitIdentity(matrixID string, deviceId string, memberID string) LiveKitIdentity {
	// Create a deterministic LiveKit identity based on user ID and device ID
	// to ensure uniqueness and avoid collisions.
	hash := sha256.Sum256(marshalStrings([]string{matrixID, deviceId, memberID}))
	return LiveKitIdentity(unpaddedBase64.EncodeToString(hash[:]))
}

var CreateLiveKitRoom = func(ctx context.Context, liveKitAuth *LiveKitAuth, room LiveKitRoomAlias, matrixUser string, lkIdentity LiveKitIdentity) error {
	roomClient := newRoomServiceClient(liveKitAuth.lkUrl, liveKitAuth.key, liveKitAuth.secret)
	creationStart := time.Now().Unix()

	const emptyTimeoutSecs = 5 * 60 // 5 minutes: keep room open if no one joins
	const departureTimeoutSecs = 20 // 20 seconds: keep room after everyone leaves

	lkRoom, err := roomClient.CreateRoom(
		ctx,
		&livekit.CreateRoomRequest{
			Name:             string(room),
			EmptyTimeout:     emptyTimeoutSecs,
			DepartureTimeout: departureTimeoutSecs,
			MaxParticipants:  0, // 0 == no limitation
		},
	)

	if err != nil {
		slog.Error(
			"CreateLiveKitRoom: Error creating room",
			"room", room,
			"lkId", lkIdentity,
			"matrixUser", matrixUser,
			"access", "full",
			"err", err,
		)
		return fmt.Errorf("unable to create room %s: %w", room, err)
	}

	isNewRoom := lkRoom.GetCreationTime() >= creationStart && lkRoom.GetCreationTime() <= time.Now().Unix()
	slog.Info(
		fmt.Sprintf("CreateLiveKitRoom: %s Room", map[bool]string{true: "Created", false: "Using"}[isNewRoom]),
		"room", room,
		"roomSid", lkRoom.Sid,
		"lkId", lkIdentity,
		"matrixUser", matrixUser,
		"access", "full",
	)

	return nil
}

// LiveKitGetParticipant checks whether the given identity is currently present
// in the given LiveKit room.  Returns nil if the participant is present, an
// error otherwise (including "not found").  Used by startParticipantLookup
// for both Phase-1 (initial lookup) and Phase-2 (periodic sanity check).
var LiveKitGetParticipant = func(
	ctx context.Context,
	lkAuth LiveKitAuth,
	room LiveKitRoomAlias,
	identity LiveKitIdentity,
) error {
	roomClient := newRoomServiceClient(lkAuth.lkUrl, lkAuth.key, lkAuth.secret)
	_, err := roomClient.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     string(room),
		Identity: string(identity),
	})
	return err
}

// ExecuteDelayedEventAction POSTs the given action (restart/send) to the Matrix
// CS-API for delayID and returns the HTTP status code.  The returned error
// drives retries when callers wrap the call in backoff.Retry:
//
//   - 200/204 on either action, and 404 on ActionSend (per MSC4140: event
//     already sent or cancelled) return a nil error.
//   - 429 with a usable Retry-After returns a *backoff.RetryAfterError so
//     backoff honours the server's hint.
//   - 429 without a usable Retry-After, 502, and transport errors return a
//     plain error so backoff retries with its default interval.
//
// The status code is returned even on error so callers can log it; 0 means no
// response was received (URL build failure or transport error).
var ExecuteDelayedEventAction = func(csAPIURL string, delayID string, action DelayEventAction) (int, error) {
	// url.JoinPath path-escapes delayID, preventing path-traversal attacks since
	// delayID is attacker-controlled.  action is a typed constant and safe.
	endpoint, err := url.JoinPath(csAPIURL, DelayedEventsEndpoint, delayID, string(action))
	if err != nil {
		return 0, fmt.Errorf("ExecuteDelayedEventAction: invalid URL: %w", err)
	}

	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Post(endpoint, "application/json", bytes.NewBufferString("{}"))
	if err != nil {
		slog.Debug("ExecuteDelayedEventAction", "url", endpoint, "err", err)
		return 0, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			slog.Error("failed to close response body", "err", cerr)
		}
	}()

	slog.Debug("ExecuteDelayedEventAction", "url", endpoint, "StatusCode", resp.StatusCode)

	switch {
	case action == ActionSend && resp.StatusCode == http.StatusNotFound:
		// MSC-4140: 404 on send means the event was already sent or cancelled.
		return resp.StatusCode, nil

	case resp.StatusCode == http.StatusBadGateway:
		// Transient: CS API restart / load-balancer hiccup.  Let backoff retry.
		return resp.StatusCode, fmt.Errorf("CS API temporarily unavailable (http status code 502)")

	case resp.StatusCode == http.StatusTooManyRequests:
		// Honour Retry-After if it parses as delta-seconds or HTTP-date
		// (RFC 7231 §7.1.3); otherwise return a plain transient error so
		// backoff.Retry retries on its default schedule.
		retryAfter := resp.Header.Get("Retry-After")
		if seconds, err := strconv.Atoi(retryAfter); err == nil {
			if seconds < 0 {
				seconds = 0
			}
			return resp.StatusCode, backoff.RetryAfter(seconds)
		}
		if t, err := http.ParseTime(retryAfter); err == nil {
			d := time.Until(t)
			if d < 0 {
				d = 0
			}
			return resp.StatusCode, backoff.RetryAfter(int(d.Seconds()))
		}
		return resp.StatusCode, fmt.Errorf("CS API temporarily unavailable (http status code 429)")
	}

	return resp.StatusCode, nil
}
