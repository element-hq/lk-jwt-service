package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
		return nil, errors.New("failed to look up user info")
	}
	return &userinfo, nil
}

var unpaddedBase64 = base64.StdEncoding.WithPadding(base64.NoPadding)

func CreateLiveKitRoomAlias(matrixRoom string, matrixRtcSlot string) LiveKitRoomAlias {
	// Create a deterministic LiveKit room alias based on Matrix room ID and slot ID
	// to ensure uniqueness and avoid collisions.
	lkRoomAliasHash := sha256.Sum256([]byte(matrixRoom + "|" + matrixRtcSlot))
	return LiveKitRoomAlias(unpaddedBase64.EncodeToString(lkRoomAliasHash[:]))
}

func CreateLiveKitIdentity(matrixID string, deviceId string, memberID string) LiveKitIdentity {
	// Create a deterministic LiveKit identity based on user ID and device ID
	// to ensure uniqueness and avoid collisions.
	lkIdentityRaw := matrixID + "|" + deviceId + "|" + memberID
	lkIdentityHash := sha256.Sum256([]byte(lkIdentityRaw))
	return LiveKitIdentity(unpaddedBase64.EncodeToString(lkIdentityHash[:]))
}

var CreateLiveKitRoom = func(ctx context.Context, liveKitAuth *LiveKitAuth, room LiveKitRoomAlias, matrixUser string, lkIdentity LiveKitIdentity) error {
	roomClient := lksdk.NewRoomServiceClient(liveKitAuth.lkUrl, liveKitAuth.key, liveKitAuth.secret)
	creationStart := time.Now().Unix()
	lkRoom, err := roomClient.CreateRoom(
		ctx,
		&livekit.CreateRoomRequest{
			Name:             string(room),
			EmptyTimeout:     5 * 60, // 5 Minutes to keep the room open if no one joins
			DepartureTimeout: 20,     // number of seconds to keep the room after everyone leaves
			MaxParticipants:  0,      // 0 == no limitation
		},
	)

	if err != nil {
		return fmt.Errorf("unable to create room %s: %w", room, err)
	}

	// Log the room creation time and the user info
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

var LiveKitParticipantLookup = func(ctx context.Context, lkAuth LiveKitAuth, lkRoomAlias LiveKitRoomAlias, lkId LiveKitIdentity, ch chan SFUMessage) (bool, error) {
	roomClient := lksdk.NewRoomServiceClient(
		lkAuth.lkUrl,
		lkAuth.key, 
		lkAuth.secret,
	)

	_, err := roomClient.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     string(lkRoomAlias),
		Identity: string(lkId),
	})

	if err == nil {
		ch <- SFUMessage{Type: ParticipantLookupSuccessful, LiveKitIdentity: lkId}
	}

	return (err==nil), err
}

var ExecuteDelayedEventAction = func(baseUrl string, delayID string, action DelayEventAction) (*http.Response, error) {

	url := fmt.Sprintf("%s%s/%s/%s", baseUrl, DelayedEventsEndpoint, delayID, action)
	var jsonStr = []byte(`{}`)

	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewBuffer(jsonStr))	

	if err != nil{
		slog.Debug("ExecuteDelayedEventAction", "time", time.Now(), "url", url, "err", err)
		return resp, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Error("failed to close response body", "err", err)
		}
	}()

	slog.Debug("ExecuteDelayedEventAction", "time", time.Now(), "url", url, "StatusCode", resp.StatusCode, "err", err)

	// As noted here
	// https://github.com/matrix-org/matrix-spec-proposals/blob/toger5/expiring-events-keep-alive/proposals/4140-delayed-events-futures.md#managing-delayed-events
	// http.StatusNotFound (404) indicates that the delayed event is already sent or does not exist.
	if action == ActionSend && resp.StatusCode == http.StatusNotFound {
		return resp, nil
	}

	// TODO: Should we ignore non-retriable errors here?
	//   - Reason: The purpose of the exponential backoff is to retry in case the server is temporarily unavailable.
	//
	// Return a Permanent error to stop retrying.
	// For this HTTP example, client errors are non-retriable.
	/*if resp.StatusCode == 400 {
		return "", backoff.Permanent(errors.New("bad request"))
	}*/

	// If we are being rate limited, return a RetryAfter to specify how long to wait.
	// This will also reset the backoff policy.
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := resp.Header.Get("Retry-After")

		// 1st attempt: Parse as an integer (seconds)
		if seconds, err := strconv.Atoi(retryAfter); err == nil {
			return nil, backoff.RetryAfter(seconds)
		}

		// 2nd attempt: Parse as HTTP date (e.g. Wed, 21 Oct 2015 07:28:00 GMT)
		if date, err := http.ParseTime(retryAfter); err == nil {
			duration := time.Until(date)
			if duration > 0 {
				return nil, backoff.RetryAfter(int(duration.Seconds()))
			}
			return nil, backoff.RetryAfter(0) // Date is in the past
		}		
	}
	
	return resp, err
}