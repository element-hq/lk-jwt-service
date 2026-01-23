package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
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

var helperLiveKitParticipantLookup = func(ctx context.Context, lkAuth LiveKitAuth, lkRoomAlias LiveKitRoomAlias, lkId LiveKitIdentity, ch chan SFUMessage) (bool, error) {
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

var helperExecuteDelayedEventAction = func(baseUrl string, delayID string, action DelayEventAction) (*http.Response, error) {

    url := fmt.Sprintf("%s%s/%s/%s", baseUrl, DelayedEventsEndpoint, delayID, action)
    var jsonStr = []byte(`{}`)

    http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
    client := &http.Client{Timeout: 1 * time.Second}
    resp, err := client.Post(url, "application/json", bytes.NewBuffer(jsonStr))
    slog.Debug("helperExecuteDelayedEventAction", "time", time.Now(), "url", url, "StatusCode", resp.StatusCode, "err", err)

    if err != nil{
        return resp, err
    }
    defer func() {
        if err := resp.Body.Close(); err != nil {
            slog.Error("failed to close response body", "err", err)
        }
    }()

    // https://go.dev/src/net/http/status.go
    
    // In case the delayed event is already send we get a 404 http.StatusNotFound
    if action == ActionSend && resp.StatusCode == http.StatusNotFound {
        return resp, nil
    }

    // In case on non-retriable error, return Permanent error to stop retrying.
    // For this HTTP example, client errors are non-retriable.
    /*if resp.StatusCode == 400 {
        return "", backoff.Permanent(errors.New("bad request"))
    }*/

    // If we are being rate limited, return a RetryAfter to specify how long to wait.
    // This will also reset the backoff policy.
    if resp.StatusCode == http.StatusTooManyRequests {
        seconds, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64)
        if err == nil {
            return nil, backoff.RetryAfter(int(seconds))
        }
    }
    
    return resp, err
}