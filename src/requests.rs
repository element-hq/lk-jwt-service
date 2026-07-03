// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// requests.rs: HTTP request / response DTOs used by the handler —
// SfuRequest, DelegateDelayedLeaveRequest, LegacySfuRequest,
// SfuResponse, MatrixErrorResponse — together with their validate()
// methods. Each type lives next to its validator.

use serde::{Deserialize, Serialize};
use tracing::error;

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MatrixRtcMemberType {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub claimed_user_id: String,
    #[serde(default)]
    pub claimed_device_id: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OpenIdTokenType {
    #[serde(default)]
    pub access_token: String,
    #[serde(default)]
    pub token_type: String,
    #[serde(default)]
    pub matrix_server_name: String,
    #[serde(default)]
    pub expires_in: i64,
}

/// Deprecated: LegacySfuRequest is the request body for /sfu/get, the
/// pre-Matrix-2.0 endpoint. Remove once all in-the-wild clients have
/// migrated to /get_token (SfuRequest).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LegacySfuRequest {
    #[serde(default)]
    pub room: String,
    #[serde(default)]
    pub openid_token: OpenIdTokenType,
    #[serde(default)]
    pub device_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delay_id: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub delay_timeout: i64,
    /// Deprecated and ignored to allow transition. Server discovery or explicit
    /// overrides via LIVEKIT_CS_API_URL_OVERRIDES will be used instead.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delay_cs_api_url: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SfuRequest {
    #[serde(default)]
    pub room_id: String,
    #[serde(default)]
    pub slot_id: String,
    #[serde(default)]
    pub openid_token: OpenIdTokenType,
    #[serde(default)]
    pub member: MatrixRtcMemberType,
    // Deprecated.
    // TODO: These were only supported in an earlier version of the MSC. Later versions
    // split token request and disconnect delegation into separate endpoints. We should
    // remove these parameters and the related code at some point.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delay_id: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub delay_timeout: i64,
    /// Deprecated and ignored to allow transition. Server discovery or explicit
    /// overrides via LIVEKIT_CS_API_URL_OVERRIDES will be used instead.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delay_cs_api_url: String,
}

fn is_zero(v: &i64) -> bool {
    *v == 0
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct SfuResponse {
    pub url: String,
    pub jwt: String,
}

/// DelegateDelayedLeaveRequest is the body of POST /delegate_delayed_leave.
/// It is used when the client is already connected to the SFU and wants to
/// hand over the delayed disconnect event after the fact — i.e. no JWT is
/// needed and the LiveKit room already exists.
///
/// All three delayed-event fields are mandatory (unlike SfuRequest where they
/// are optional). The participant is assumed to be already present on the SFU,
/// so the participant-lookup task uses its backoff to confirm presence
/// rather than waiting for the webhook (which has already fired).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DelegateDelayedLeaveRequest {
    #[serde(default)]
    pub room_id: String,
    #[serde(default)]
    pub slot_id: String,
    #[serde(default)]
    pub openid_token: OpenIdTokenType,
    #[serde(default)]
    pub member: MatrixRtcMemberType,
    #[serde(default)]
    pub delay_id: String,
    #[serde(default)]
    pub delay_timeout: i64,
    /// Deprecated and ignored to allow transition. Server discovery or explicit
    /// overrides via LIVEKIT_CS_API_URL_OVERRIDES will be used instead.
    #[serde(default)]
    pub delay_cs_api_url: String,
}

impl DelegateDelayedLeaveRequest {
    pub fn validate(&self) -> Result<(), MatrixErrorResponse> {
        if self.room_id.is_empty() || self.slot_id.is_empty() {
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "The request body is missing `room_id` or `slot_id`".into(),
            });
        }
        if self.member.id.is_empty()
            || self.member.claimed_user_id.is_empty()
            || self.member.claimed_device_id.is_empty()
        {
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "The request body `member` is missing `id`, `claimed_user_id` or `claimed_device_id`".into(),
            });
        }
        if self.openid_token.access_token.is_empty()
            || self.openid_token.matrix_server_name.is_empty()
        {
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "The request body `openid_token` is missing `access_token` or `matrix_server_name`".into(),
            });
        }
        if self.delay_id.is_empty() || self.delay_timeout <= 0 {
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "The request body is missing `delay_id` or `delay_timeout`".into(),
            });
        }
        Ok(())
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct DelegateDelayedLeaveResponse {}

/// A Matrix-style error response. Doubles as the error type carried through
/// request processing so that HTTP handlers can map it onto the wire.
#[derive(Debug, Clone, thiserror::Error)]
#[error("{err}")]
pub struct MatrixErrorResponse {
    pub status: u16,
    pub errcode: String,
    pub err: String,
}

/// The wire format of a Matrix error body ({"errcode": ..., "error": ...}).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MatrixErrorBody {
    pub errcode: String,
    #[serde(rename = "error")]
    pub error: String,
}

impl SfuRequest {
    pub fn validate(&self) -> Result<(), MatrixErrorResponse> {
        if self.room_id.is_empty() || self.slot_id.is_empty() {
            error!(room_id = %self.room_id, slot_id = %self.slot_id, "Missing room_id or slot_id");
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "The request body is missing `room_id` or `slot_id`".into(),
            });
        }
        if self.member.id.is_empty()
            || self.member.claimed_user_id.is_empty()
            || self.member.claimed_device_id.is_empty()
        {
            error!(member = ?self.member, "Handler -> SfuRequest: Missing member parameters");
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "The request body `member` is missing a `id`, `claimed_user_id` or `claimed_device_id`".into(),
            });
        }
        if self.openid_token.access_token.is_empty()
            || self.openid_token.matrix_server_name.is_empty()
        {
            error!(openid_token = ?self.openid_token, "Handler -> SfuRequest: Missing OpenID token parameters");
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "The request body `openid_token` is missing a `access_token` or `matrix_server_name`".into(),
            });
        }

        let all_delayed_event_params_present = !self.delay_id.is_empty() && self.delay_timeout > 0;
        let at_least_one_delayed_event_param_present =
            !self.delay_id.is_empty() || self.delay_timeout > 0;
        if at_least_one_delayed_event_param_present && !all_delayed_event_params_present {
            error!(
                delay_id = %self.delay_id,
                delay_timeout = self.delay_timeout,
                "Handler -> SfuRequest: Missing delayed event delegation parameters"
            );
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "The request body is missing `delay_id` or `delay_timeout`".into(),
            });
        }

        Ok(())
    }
}

impl LegacySfuRequest {
    pub fn validate(&self) -> Result<(), MatrixErrorResponse> {
        if self.room.is_empty() {
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "Missing room parameter".into(),
            });
        }
        if self.openid_token.access_token.is_empty()
            || self.openid_token.matrix_server_name.is_empty()
        {
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "Missing OpenID token parameters".into(),
            });
        }
        let all_delayed_event_params_present = !self.delay_id.is_empty() && self.delay_timeout > 0;
        let at_least_one_delayed_event_param_present =
            !self.delay_id.is_empty() || self.delay_timeout > 0;
        if at_least_one_delayed_event_param_present && !all_delayed_event_params_present {
            error!(
                delay_id = %self.delay_id,
                delay_timeout = self.delay_timeout,
                "Handler -> SfuRequest: Missing delayed event delegation parameters"
            );
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "The request body is missing `delay_id` or `delay_timeout`".into(),
            });
        }
        Ok(())
    }
}

// ─────────────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;

    /// Checks that err is a MatrixErrorResponse with the expected error code.
    fn assert_validation_error(err: Result<(), MatrixErrorResponse>, want_errcode: &str) {
        match err {
            Ok(()) => panic!("expected validation error, got Ok"),
            Err(matrix_err) => {
                assert_eq!(
                    matrix_err.errcode, want_errcode,
                    "errcode = {:?}, want {:?}",
                    matrix_err.errcode, want_errcode
                );
            }
        }
    }

    /// Verifies the Display (Error) string of MatrixErrorResponse.
    #[test]
    fn test_matrix_error_response_error() {
        let err = MatrixErrorResponse {
            status: 400,
            errcode: "M_BAD_JSON".into(),
            err: "bad input".into(),
        };
        assert_eq!(err.to_string(), "bad input");
    }

    // ── SfuRequest / LegacySfuRequest validation ──────────────────────────────

    /// Verifies that providing only some delayed-event parameters returns
    /// M_BAD_JSON.
    #[test]
    fn test_legacy_sfu_request_validate_delayed_event_partial_params() {
        for (name, delay_id, timeout, cs_url) in [
            ("only delay_id", "did", 0, ""),
            ("only timeout", "", 1000, ""),
            ("delay_id + cs_api_url", "did", 0, "https://example.com"),
            ("timeout + cs_api_url", "", 1000, "https://example.com"),
        ] {
            let req = LegacySfuRequest {
                room: "!r:x".into(),
                device_id: "DEVICE1234".into(),
                openid_token: OpenIdTokenType {
                    access_token: "tok".into(),
                    matrix_server_name: "x".into(),
                    ..Default::default()
                },
                delay_id: delay_id.into(),
                delay_timeout: timeout,
                delay_cs_api_url: cs_url.into(),
            };
            assert!(
                req.validate().is_err(),
                "{name}: expected validation error for partial delayed-event params, got Ok"
            );
        }
    }

    #[test]
    fn test_legacy_sfu_request_validate_cs_api_url_ignored() {
        for (name, delay_id, timeout, cs_url) in [
            ("delay parameters but no url", "did", 1000, ""),
            ("no delay parameters and no url", "", 0, ""),
        ] {
            let req = LegacySfuRequest {
                room: "!r:x".into(),
                device_id: "DEVICE1234".into(),
                openid_token: OpenIdTokenType {
                    access_token: "tok".into(),
                    matrix_server_name: "x".into(),
                    ..Default::default()
                },
                delay_id: delay_id.into(),
                delay_timeout: timeout,
                delay_cs_api_url: cs_url.into(),
            };
            assert!(
                req.validate().is_ok(),
                "{name}: expected no validation error for delayed-event params without URL"
            );
        }
    }

    /// Verifies that providing only some delayed-event parameters returns
    /// M_BAD_JSON.
    #[test]
    fn test_sfu_request_validate_delayed_event_partial_params() {
        for (name, delay_id, timeout, cs_url) in [
            ("only delay_id", "did", 0, ""),
            ("only timeout", "", 1000, ""),
            ("delay_id + cs_api_url", "did", 0, "https://example.com"),
            ("timeout + cs_api_url", "", 1000, "https://example.com"),
        ] {
            let req = SfuRequest {
                room_id: "!r:x".into(),
                slot_id: "s".into(),
                openid_token: OpenIdTokenType {
                    access_token: "tok".into(),
                    matrix_server_name: "x".into(),
                    ..Default::default()
                },
                member: MatrixRtcMemberType {
                    id: "id".into(),
                    claimed_user_id: "@u:x".into(),
                    claimed_device_id: "d".into(),
                },
                delay_id: delay_id.into(),
                delay_timeout: timeout,
                delay_cs_api_url: cs_url.into(),
            };
            assert!(
                req.validate().is_err(),
                "{name}: expected validation error for partial delayed-event params, got Ok"
            );
        }
    }

    #[test]
    fn test_sfu_request_validate_cs_api_url_ignored() {
        for (name, delay_id, timeout, cs_url) in [
            ("delay parameters but no url", "did", 1000, ""),
            ("no delay parameters and no url", "", 0, ""),
        ] {
            let req = SfuRequest {
                room_id: "!r:x".into(),
                slot_id: "s".into(),
                openid_token: OpenIdTokenType {
                    access_token: "tok".into(),
                    matrix_server_name: "x".into(),
                    ..Default::default()
                },
                member: MatrixRtcMemberType {
                    id: "id".into(),
                    claimed_user_id: "@u:x".into(),
                    claimed_device_id: "d".into(),
                },
                delay_id: delay_id.into(),
                delay_timeout: timeout,
                delay_cs_api_url: cs_url.into(),
            };
            assert!(
                req.validate().is_ok(),
                "{name}: expected no validation error for delayed-event params without URL"
            );
        }
    }

    // ── DelegateDelayedLeaveRequest::validate() ───────────────────────────────

    pub(crate) fn valid_delegate_delayed_leave_request() -> DelegateDelayedLeaveRequest {
        DelegateDelayedLeaveRequest {
            room_id: "!testRoom:example.com".into(),
            slot_id: "m.call#ROOM".into(),
            openid_token: OpenIdTokenType {
                access_token: "test-token".into(),
                matrix_server_name: "example.com".into(),
                ..Default::default()
            },
            member: MatrixRtcMemberType {
                id: "member-id".into(),
                claimed_user_id: "@user:example.com".into(),
                claimed_device_id: "device-id".into(),
            },
            delay_id: "syd_delay123".into(),
            delay_timeout: 30000, // 30 s in ms
            delay_cs_api_url: "https://matrix.example.com".into(),
        }
    }

    #[test]
    fn test_delegate_delayed_leave_request_validate_valid() {
        let req = valid_delegate_delayed_leave_request();
        assert!(req.validate().is_ok());
    }

    #[test]
    fn test_delegate_delayed_leave_request_validate_missing_room_id() {
        let mut req = valid_delegate_delayed_leave_request();
        req.room_id = String::new();
        assert_validation_error(req.validate(), "M_BAD_JSON");
    }

    #[test]
    fn test_delegate_delayed_leave_request_validate_missing_slot_id() {
        let mut req = valid_delegate_delayed_leave_request();
        req.slot_id = String::new();
        assert_validation_error(req.validate(), "M_BAD_JSON");
    }

    #[test]
    fn test_delegate_delayed_leave_request_validate_missing_member_fields() {
        type Mutator = fn(&mut DelegateDelayedLeaveRequest);
        let cases: Vec<(&str, Mutator)> = vec![
            ("missing ID", |r| r.member.id = String::new()),
            ("missing ClaimedUserID", |r| {
                r.member.claimed_user_id = String::new()
            }),
            ("missing ClaimedDeviceID", |r| {
                r.member.claimed_device_id = String::new()
            }),
        ];
        for (name, mutate) in cases {
            let mut req = valid_delegate_delayed_leave_request();
            mutate(&mut req);
            let result = req.validate();
            assert!(result.is_err(), "{name}: expected validation error");
            assert_validation_error(result, "M_BAD_JSON");
        }
    }

    #[test]
    fn test_delegate_delayed_leave_request_validate_missing_openid_token() {
        type Mutator = fn(&mut DelegateDelayedLeaveRequest);
        let cases: Vec<(&str, Mutator)> = vec![
            ("missing AccessToken", |r| {
                r.openid_token.access_token = String::new()
            }),
            ("missing MatrixServerName", |r| {
                r.openid_token.matrix_server_name = String::new()
            }),
        ];
        for (name, mutate) in cases {
            let mut req = valid_delegate_delayed_leave_request();
            mutate(&mut req);
            let result = req.validate();
            assert!(result.is_err(), "{name}: expected validation error");
            assert_validation_error(result, "M_BAD_JSON");
        }
    }

    #[test]
    fn test_delegate_delayed_leave_request_validate_missing_delayed_event_params() {
        type Mutator = fn(&mut DelegateDelayedLeaveRequest);
        let cases: Vec<(&str, Mutator)> = vec![
            ("missing DelayId", |r| r.delay_id = String::new()),
            ("zero DelayTimeout", |r| r.delay_timeout = 0),
            ("negative DelayTimeout", |r| r.delay_timeout = -1),
        ];
        for (name, mutate) in cases {
            let mut req = valid_delegate_delayed_leave_request();
            mutate(&mut req);
            let result = req.validate();
            assert!(result.is_err(), "{name}: expected validation error");
            assert_validation_error(result, "M_BAD_JSON");
        }
    }

    #[test]
    fn test_delegate_delayed_leave_request_validate_missing_cs_api_url_ignored() {
        let mut req = valid_delegate_delayed_leave_request();
        req.delay_cs_api_url = String::new();
        assert!(
            req.validate().is_ok(),
            "expected no error for valid request without CS API URL"
        );
    }
}
