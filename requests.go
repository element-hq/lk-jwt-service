// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// requests.go: HTTP request / response DTOs used by the handler —
// SFURequest, DelegateDelayedLeaveRequest, LegacySFURequest,
// SFUResponse, MatrixErrorResponse — together with their Validate()
// methods. Each type lives next to its validator (idiomatic Go).

package main

import (
	"log/slog"
	"net/http"
)

type MatrixRTCMemberType struct {
	ID              string `json:"id"`
	ClaimedUserID   string `json:"claimed_user_id"`
	ClaimedDeviceID string `json:"claimed_device_id"`
}

type OpenIDTokenType struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	MatrixServerName string `json:"matrix_server_name"`
	ExpiresIn        int    `json:"expires_in"`
}

// Deprecated: LegacySFURequest is the request body for /sfu/get, the
// pre-Matrix-2.0 endpoint. Remove once all in-the-wild clients have
// migrated to /get_token (SFURequest).
type LegacySFURequest struct {
	Room          string          `json:"room"`
	OpenIDToken   OpenIDTokenType `json:"openid_token"`
	DeviceID      string          `json:"device_id"`
	DelayId       string          `json:"delay_id,omitempty"`
	DelayTimeout  int             `json:"delay_timeout,omitempty"`
	DelayCsApiUrl string          `json:"delay_cs_api_url,omitempty"`
}

type SFURequest struct {
	RoomID        string              `json:"room_id"`
	SlotID        string              `json:"slot_id"`
	OpenIDToken   OpenIDTokenType     `json:"openid_token"`
	Member        MatrixRTCMemberType `json:"member"`
	DelayId       string              `json:"delay_id,omitempty"`
	DelayTimeout  int                 `json:"delay_timeout,omitempty"`
	DelayCsApiUrl string              `json:"delay_cs_api_url,omitempty"`
}

type SFUResponse struct {
	URL string `json:"url"`
	JWT string `json:"jwt"`
}

// DelegateDelayedLeaveRequest is the body of POST /delegate_delayed_leave.
// It is used when the client is already connected to the SFU and wants to
// hand over the delayed disconnect event after the fact — i.e. no JWT is
// needed and the LiveKit room already exists.
//
// All three delayed-event fields are mandatory (unlike SFURequest where they
// are optional). The participant is assumed to be already present on the SFU,
// so the participant-lookup goroutine uses its backoff to confirm presence
// rather than waiting for the webhook (which has already fired).
type DelegateDelayedLeaveRequest struct {
	RoomID        string              `json:"room_id"`
	SlotID        string              `json:"slot_id"`
	OpenIDToken   OpenIDTokenType     `json:"openid_token"`
	Member        MatrixRTCMemberType `json:"member"`
	DelayId       string              `json:"delay_id"`
	DelayTimeout  int                 `json:"delay_timeout"`
	DelayCsApiUrl string              `json:"delay_cs_api_url"`
}

func (r *DelegateDelayedLeaveRequest) Validate() error {
	if r.RoomID == "" || r.SlotID == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body is missing `room_id` or `slot_id`",
		}
	}
	if r.Member.ID == "" || r.Member.ClaimedUserID == "" || r.Member.ClaimedDeviceID == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body `member` is missing `id`, `claimed_user_id` or `claimed_device_id`",
		}
	}
	if r.OpenIDToken.AccessToken == "" || r.OpenIDToken.MatrixServerName == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body `openid_token` is missing `access_token` or `matrix_server_name`",
		}
	}
	if r.DelayId == "" || r.DelayTimeout <= 0 || r.DelayCsApiUrl == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body is missing `delay_id`, `delay_timeout` or `delay_cs_api_url`",
		}
	}
	return nil
}

type MatrixErrorResponse struct {
	Status  int
	ErrCode string
	Err     string
}

func (e *MatrixErrorResponse) Error() string {
	return e.Err
}

func (r *SFURequest) Validate() error {
	if r.RoomID == "" || r.SlotID == "" {
		slog.Error("Missing room_id or slot_id", "room_id", r.RoomID, "slot_id", r.SlotID)
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body is missing `room_id` or `slot_id`",
		}
	}
	if r.Member.ID == "" || r.Member.ClaimedUserID == "" || r.Member.ClaimedDeviceID == "" {
		slog.Error("Handler -> SFURequest: Missing member parameters", "Member", r.Member)
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body `member` is missing a `id`, `claimed_user_id` or `claimed_device_id`",
		}
	}
	if r.OpenIDToken.AccessToken == "" || r.OpenIDToken.MatrixServerName == "" {
		slog.Error("Handler -> SFURequest: Missing OpenID token parameters:", "OpenIDToken", r.OpenIDToken)
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body `openid_token` is missing a `access_token` or `matrix_server_name`",
		}
	}

	allDelayedEventParamsPresent := r.DelayId != "" && r.DelayTimeout > 0 && r.DelayCsApiUrl != ""
	atLeastOneDelayedEventParamPresent := r.DelayId != "" || r.DelayTimeout > 0 || r.DelayCsApiUrl != ""
	if atLeastOneDelayedEventParamPresent && !allDelayedEventParamsPresent {
		slog.Error("Handler -> SFURequest: Missing delayed event delegation parameters",
			"delayId", r.DelayId,
			"delayTimeout", r.DelayTimeout,
			"csApiUrl", r.DelayCsApiUrl,
		)
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body is missing `delay_id`, `delay_timeout` or `delay_cs_api_url`",
		}
	}

	return nil
}

func (r *LegacySFURequest) Validate() error {
	if r.Room == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "Missing room parameter",
		}
	}
	if r.OpenIDToken.AccessToken == "" || r.OpenIDToken.MatrixServerName == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "Missing OpenID token parameters",
		}
	}
	allDelayedEventParamsPresent := r.DelayId != "" && r.DelayTimeout > 0 && r.DelayCsApiUrl != ""
	atLeastOneDelayedEventParamPresent := r.DelayId != "" || r.DelayTimeout > 0 || r.DelayCsApiUrl != ""
	if atLeastOneDelayedEventParamPresent && !allDelayedEventParamsPresent {
		slog.Error("Handler -> SFURequest: Missing delayed event delegation parameters",
			"delayId", r.DelayId,
			"delayTimeout", r.DelayTimeout,
			"csApiUrl", r.DelayCsApiUrl,
		)
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body is missing `delay_id`, `delay_timeout` or `delay_cs_api_url`",
		}
	}
	return nil
}
