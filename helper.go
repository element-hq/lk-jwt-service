// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// helper.go: cross-cutting helpers shared by request handlers and the
// delayed-event manager — ID generation, LiveKit SDK wrappers, Matrix
// CS-API calls.  The exported `var X = func(...)` patterns exist so
// tests can swap them; this is slated to move to interface-based
// injection in a later step.

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
	"github.com/twitchtv/twirp"
)

// errParticipantAbsent is returned to backoff.Retry from
// startParticipantLookup's Phase-1 loop so it keeps polling while the SFU
// confirms the participant is not present yet. (Not a transport failure)
var errParticipantAbsent = errors.New("livekit: participant not currently present")

// errDelayedEventNotFound is returned by ExecuteDelayedEventAction when a
// 404 comes back on ActionRestart.
var errDelayedEventNotFound = errors.New("CS API: delayed event not found")

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

// LiveKitRoomAliasFor returns the deterministic LiveKit room alias for a
// given (Matrix room ID, MatrixRTC slot) pair.
func LiveKitRoomAliasFor(matrixRoom string, matrixRtcSlot string) LiveKitRoomAlias {
	hash := sha256.Sum256(marshalStrings([]string{matrixRoom, matrixRtcSlot}))
	return LiveKitRoomAlias(unpaddedBase64.EncodeToString(hash[:]))
}

// LiveKitIdentityFor returns the deterministic LiveKit identity for a given
// (Matrix user ID, device ID, MatrixRTC member ID) tuple.
func LiveKitIdentityFor(matrixID string, deviceId string, memberID string) LiveKitIdentity {
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
	slog.Info("CreateLiveKitRoom",
		"room_status", map[bool]string{true: "created", false: "reused"}[isNewRoom],
		"room", room,
		"roomSid", lkRoom.Sid,
		"lkId", lkIdentity,
		"matrixUser", matrixUser,
		"access", "full",
	)

	return nil
}

// LiveKitParticipantExists reports whether the given identity is currently
// present in the given LiveKit room.
//
//   - (true, nil)   participant present.
//   - (false, nil)  participant confirmed absent (SFU returned NotFound).
//   - (false, err)  transport / auth / server error — presence unknown.
//
// Used by startParticipantLookup for both Phase-1 (initial lookup) and
// Phase-2 (periodic sanity check).
var LiveKitParticipantExists = func(
	ctx context.Context,
	lkAuth LiveKitAuth,
	room LiveKitRoomAlias,
	identity LiveKitIdentity,
) (bool, error) {
	roomClient := newRoomServiceClient(lkAuth.lkUrl, lkAuth.key, lkAuth.secret)
	_, err := roomClient.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     string(room),
		Identity: string(identity),
	})
	if err == nil {
		return true, nil
	}
	var twirpErr twirp.Error
	if errors.As(err, &twirpErr) && twirpErr.Code() == twirp.NotFound {
		return false, nil
	}
	return false, err
}

// ExecuteDelayedEventAction POSTs the given action (restart/send) to the
// Matrix CS-API for delayID.  Explicit-success contract:
//
//   - (200/204, nil)                       success — action took effect
//   - (404, nil)          ActionSend       MSC-4140: already sent / cancelled
//   - (404, errDelayedEventNotFound)
//     ActionRestart                        delayed event no longer present
//   - (5xx, transient err)                 CS API hiccup, let backoff retry
//   - (429, *backoff.RetryAfterError)      Retry-After header OR Matrix
//     retry_after_ms body field
//   - (429, transient err)                 no usable retry hint, default backoff
//   - (status, "unexpected status" err)    any other code — treated as
//     transient (backoff retries
//     until WithMaxElapsedTime)
//   - (0, transport err)                   URL build or network error
//
// The status code is returned even on error so callers can log it; 0 means
// no response was received.
var ExecuteDelayedEventAction = func(csAPIURL string, delayID string, action DelayEventAction) (int, error) {
	// url.JoinPath path-escapes delayID, preventing path-traversal attacks since
	// delayID is attacker-controlled.  action is a typed constant and safe.
	endpoint, err := url.JoinPath(csAPIURL, DelayedEventsEndpoint, delayID, string(action))
	if err != nil {
		return 0, fmt.Errorf("ExecuteDelayedEventAction: invalid URL: %w", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
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

	slog.Debug("ExecuteDelayedEventAction", "url", endpoint, "status", resp.StatusCode)

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent:
		// Happy path: server accepted the action.
		return resp.StatusCode, nil

	case action == ActionSend && resp.StatusCode == http.StatusNotFound:
		// MSC-4140: 404 on send means the event was already sent or cancelled —
		// treat as success.
		return resp.StatusCode, nil

	case resp.StatusCode == http.StatusNotFound:
		// 404 on restart: delayed event no longer present on the homeserver.
		// Wrapped in backoff.Permanent so backoff.Retry stops immediately;
		// errors.Is unwraps through PermanentError so callers can still match
		// the sentinel.
		return resp.StatusCode, backoff.Permanent(errDelayedEventNotFound)

	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		// Any 5xx is transient (CS API restart, DB lock, load-balancer hiccup,
		// upstream timeout).  Let backoff.Retry retry on its default schedule.
		return resp.StatusCode, fmt.Errorf(
			"CS API temporarily unavailable (http status code %d)", resp.StatusCode)

	case resp.StatusCode == http.StatusTooManyRequests:
		// Prefer the standard HTTP Retry-After header (RFC 7231 §7.1.3) —
		// works for all Matrix homeservers and for any non-Matrix middlebox
		// (CDN, proxy, gateway) sitting in front.
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
		// Matrix-spec fallback: M_LIMIT_EXCEEDED carries retry_after_ms in
		// the response body.  Deprecated in spec v1.10 in favour of
		// Retry-After, but still emitted by Synapse / Dendrite / Conduit for
		// backwards compatibility — and the only signal from older
		// homeservers.  Best-effort: decode once; ignore failures.
		var mErr struct {
			ErrCode      string `json:"errcode"`
			RetryAfterMs int    `json:"retry_after_ms"`
		}
		if json.NewDecoder(resp.Body).Decode(&mErr) == nil && mErr.RetryAfterMs > 0 {
			// Ceil ms → s (e.g. 500 ms → 1 s, 1500 ms → 2 s).
			return resp.StatusCode, backoff.RetryAfter((mErr.RetryAfterMs + 999) / 1000)
		}
		// Neither header nor body had a usable hint — let backoff.Retry
		// decide.
		return resp.StatusCode, fmt.Errorf("CS API temporarily unavailable (http status code 429)")
	}

	// Anything not classified above is treated as transient — many 4xx codes
	// are genuinely retriable (408 Request Timeout, 421 Misdirected,
	// 423 Locked, 425 Too Early, …)
	return resp.StatusCode, fmt.Errorf(
		"CS API returned unexpected status: %d", resp.StatusCode)
}
