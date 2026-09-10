// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Membership enforcement (MSC4195, "Kicking users from the SFU on room
//! leave/ban") against a real Synapse and a real LiveKit SFU: participants
//! who leave, or are kicked out of, the Matrix room lose their SFU
//! connection shortly after.

use std::time::Duration;

use lk_jwt_service_e2e_tests::{
    LIVEKIT_A_SFU_ADDR, LIVEKIT_A_URL, LiveKitParticipant, SYNAPSE_A_CS_API_URL,
    SYNAPSE_A_SERVER_NAME, SYNAPSE_B_CS_API_URL, assert_stack_is_up, create_and_join_room,
    get_livekit_token, get_relayed_livekit_token, join_room_via, kick_user, leave_room,
    register_user,
};

const SLOT_ID: &str = "m.call#ROOM";

/// How long a participant gets to be thrown off the SFU after losing their
/// room membership. The stack runs the membership check every two seconds
/// (see docker-compose.yml); the rest is headroom for federation and CI.
const KICK_TIMEOUT: Duration = Duration::from_secs(20);

/// How long to keep checking that a participant who should stay actually
/// does. Covers several check intervals.
const STAY_PERIOD: Duration = Duration::from_secs(5);

/// A local user who is kicked out of the room is removed from the SFU, while
/// the remaining member stays connected.
#[tokio::test]
async fn kicked_user_is_removed_from_sfu() {
    assert_stack_is_up();

    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let bob = register_user(SYNAPSE_A_CS_API_URL, "bob", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    join_room_via(SYNAPSE_A_CS_API_URL, &bob, &room_id, SYNAPSE_A_SERVER_NAME).await;

    let alice_jwt = get_livekit_token(
        SYNAPSE_A_CS_API_URL,
        &alice,
        LIVEKIT_A_URL,
        &room_id,
        SLOT_ID,
        "e2e-member-alice",
        "E2EDEVICEALICE",
    )
    .await;
    let bob_jwt = get_livekit_token(
        SYNAPSE_A_CS_API_URL,
        &bob,
        LIVEKIT_A_URL,
        &room_id,
        SLOT_ID,
        "e2e-member-bob",
        "E2EDEVICEBOB",
    )
    .await;
    let alice_participant = LiveKitParticipant::connect(LIVEKIT_A_SFU_ADDR, &alice_jwt).await;
    let bob_participant = LiveKitParticipant::connect(LIVEKIT_A_SFU_ADDR, &bob_jwt).await;

    kick_user(SYNAPSE_A_CS_API_URL, &alice, &room_id, &bob.user_id).await;

    assert!(
        bob_participant.wait_for_disconnect(KICK_TIMEOUT).await,
        "expected bob to be removed from the SFU after being kicked from the room"
    );
    assert!(
        !alice_participant.wait_for_disconnect(STAY_PERIOD).await,
        "expected alice, still a room member, to stay connected"
    );

    alice_participant.disconnect().await;
}

/// A federated user who leaves the room is removed from the SFU they were
/// subscribed on, while the local publisher stays connected.
#[tokio::test]
async fn remote_user_leaving_is_removed_from_sfu() {
    assert_stack_is_up();

    // Alice, on hs A, creates the room and publishes on hs A's SFU.
    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    let alice_jwt = get_livekit_token(
        SYNAPSE_A_CS_API_URL,
        &alice,
        LIVEKIT_A_URL,
        &room_id,
        SLOT_ID,
        "e2e-member-alice",
        "E2EDEVICEALICE",
    )
    .await;
    let alice_participant = LiveKitParticipant::connect(LIVEKIT_A_SFU_ADDR, &alice_jwt).await;

    // Bob, on hs B, joins the room and subscribes on hs A's SFU with a token
    // relayed through his own homeserver.
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;
    join_room_via(SYNAPSE_B_CS_API_URL, &bob, &room_id, SYNAPSE_A_SERVER_NAME).await;
    let bob_jwt = get_relayed_livekit_token(
        SYNAPSE_B_CS_API_URL,
        &bob,
        SYNAPSE_A_SERVER_NAME,
        LIVEKIT_A_URL,
        &room_id,
        SLOT_ID,
        "e2e-member-bob",
        "E2EDEVICEBOB",
    )
    .await;
    let bob_participant = LiveKitParticipant::connect(LIVEKIT_A_SFU_ADDR, &bob_jwt).await;

    leave_room(SYNAPSE_B_CS_API_URL, &bob, &room_id).await;

    assert!(
        bob_participant.wait_for_disconnect(KICK_TIMEOUT).await,
        "expected bob to be removed from hs A's SFU after leaving the room"
    );
    assert!(
        !alice_participant.wait_for_disconnect(STAY_PERIOD).await,
        "expected alice, still a room member, to stay connected"
    );

    alice_participant.disconnect().await;
}

/// Once the last local user leaves, the homeserver is out of the room and
/// can no longer vouch for anyone: the LiveKit room is closed, disconnecting
/// the remaining federated subscriber.
#[tokio::test]
async fn last_local_member_leaving_removes_remaining_participants() {
    assert_stack_is_up();

    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    let alice_jwt = get_livekit_token(
        SYNAPSE_A_CS_API_URL,
        &alice,
        LIVEKIT_A_URL,
        &room_id,
        SLOT_ID,
        "e2e-member-alice",
        "E2EDEVICEALICE",
    )
    .await;
    let alice_participant = LiveKitParticipant::connect(LIVEKIT_A_SFU_ADDR, &alice_jwt).await;

    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;
    join_room_via(SYNAPSE_B_CS_API_URL, &bob, &room_id, SYNAPSE_A_SERVER_NAME).await;
    let bob_jwt = get_relayed_livekit_token(
        SYNAPSE_B_CS_API_URL,
        &bob,
        SYNAPSE_A_SERVER_NAME,
        LIVEKIT_A_URL,
        &room_id,
        SLOT_ID,
        "e2e-member-bob",
        "E2EDEVICEBOB",
    )
    .await;
    let bob_participant = LiveKitParticipant::connect(LIVEKIT_A_SFU_ADDR, &bob_jwt).await;

    // Alice hangs up and leaves the room; hs A has no member left in it.
    alice_participant.disconnect().await;
    leave_room(SYNAPSE_A_CS_API_URL, &alice, &room_id).await;

    assert!(
        bob_participant.wait_for_disconnect(KICK_TIMEOUT).await,
        "expected bob to be disconnected once hs A left the room"
    );
}
