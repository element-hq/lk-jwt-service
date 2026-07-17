// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

mod fake_homeserver;
mod fake_redis;
mod fake_sfu;
mod harness;
mod helpers;

pub use fake_homeserver::{DelayedEventRequest, FakeHomeserver, FakeUser, UserInfoRequest};
pub use fake_redis::FakeRedis;
pub use fake_sfu::{CreateRoomRequest, FakeSfu, GetParticipantRequest};
pub use harness::{LIVEKIT_KEY, LIVEKIT_SECRET, Service, ServiceConfig};
pub use helpers::{
    decode_livekit_jwt, expect_delayed_event_request, expect_delayed_event_request_count,
    expect_job_not_persisted, expect_job_persisted, expect_matrix_error,
    expect_no_delayed_event_request, expect_no_delayed_event_requests, expect_no_user_info_lookups,
    expect_user_info_lookup, livekit_identity, livekit_room_alias, send_sfu_webhook,
    wait_for_delayed_event_request, wait_for_delayed_event_request_count, wait_for_job_removed,
};
