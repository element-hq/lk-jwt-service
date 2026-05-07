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
	lkRoomAliasRawBytes, err := json.Marshal([]string{matrixRoom, matrixRtcSlot})
	if err != nil {
		panic("unreachable, probably")
	}
	lkRoomAliasRaw := string(lkRoomAliasRawBytes)
	lkRoomAliasHash := sha256.Sum256([]byte(lkRoomAliasRaw))
	return LiveKitRoomAlias(unpaddedBase64.EncodeToString(lkRoomAliasHash[:]))
}

func CreateLiveKitIdentity(matrixID string, deviceId string, memberID string) LiveKitIdentity {
	// Create a deterministic LiveKit identity based on user ID and device ID
	// to ensure uniqueness and avoid collisions.
	lkIdentityRawBytes, err := json.Marshal([]string{matrixID, deviceId, memberID})
	if err != nil {
		panic("unreachable, probably")
	}
	lkIdentityRaw := string(lkIdentityRawBytes)
	lkIdentityHash := sha256.Sum256([]byte(lkIdentityRaw))
	return LiveKitIdentity(unpaddedBase64.EncodeToString(lkIdentityHash[:]))
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

// LiveKitListParticipants returns all participants currently present in a
// LiveKit room. LiveKitRoomWorker uses this for both Phase-1 (initial lookup)
// and Phase-2 (periodic sanity check), batching all identities in one call.
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

var ExecuteDelayedEventAction = func(CsApiUrl string, delayID string, action DelayEventAction) (*http.Response, error) {
	// Use url.JoinPath so delayID is path-escaped, preventing path traversal
	// attacks (e.g. delayID = "../admin") since it is attacker-controlled.
	// action is a typed constant (ActionRestart/ActionSend) and safe by construction.
	endpoint, err := url.JoinPath(CsApiUrl, DelayedEventsEndpoint, delayID, string(action))
	if err != nil {
		return nil, fmt.Errorf("ExecuteDelayedEventAction: invalid URL: %w", err)
	}
	var jsonStr = []byte(`{}`)

	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Post(endpoint, "application/json", bytes.NewBuffer(jsonStr))

	if err != nil {
		slog.Debug("ExecuteDelayedEventAction", "time", time.Now(), "url", endpoint, "err", err)
		return resp, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Error("failed to close response body", "err", err)
		}
	}()

	slog.Debug("ExecuteDelayedEventAction", "time", time.Now(), "url", endpoint, "StatusCode", resp.StatusCode, "err", err)

	// https://github.com/matrix-org/matrix-spec-proposals/blob/toger5/expiring-events-keep-alive/proposals/4140-delayed-events-futures.md#managing-delayed-events
	// 404 means the delayed event is already sent or does not exist.
	if action == ActionSend && resp.StatusCode == http.StatusNotFound {
		return resp, nil
	}

	// Retry 502s (bad gateway) to handle transient API outages (restarts/network).
	if resp.StatusCode == http.StatusBadGateway {
		return resp, fmt.Errorf("CS API temporarily unavailable (http status code 502)")
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := resp.Header.Get("Retry-After")

		if seconds, err := strconv.Atoi(retryAfter); err == nil {
			return nil, backoff.RetryAfter(seconds)
		}

		if date, err := http.ParseTime(retryAfter); err == nil {
			duration := time.Until(date)
			if duration > 0 {
				return nil, backoff.RetryAfter(int(duration.Seconds()))
			}
			return nil, backoff.RetryAfter(0)
		}
	}

	return resp, err
}
