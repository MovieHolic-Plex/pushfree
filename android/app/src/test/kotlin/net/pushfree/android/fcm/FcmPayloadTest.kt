package net.pushfree.android.fcm

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Pure-JVM unit tests for the FCM data-only payload parser. No Firebase types,
 * no Robolectric, no network — `parseFcmPayload` is a pure function on a
 * `Map<String, String>`, which is exactly what `RemoteMessage.data` exposes.
 *
 * Covers the acceptance: happy = mock payload parses to the expected fields;
 * failure = corrupt/missing payload is ignored (returns null) rather than
 * crashing or yielding a partial entity. Field names mirror the `/1/ws` frame
 * (todo 13) and the server FCM data contract (todo 16).
 */
class FcmPayloadTest {

    // 1. Happy path: a complete, well-formed payload round-trips every field.
    @Test
    fun parses_complete_payload() {
        val data = mapOf(
            "id" to "42",
            "send_id" to "9001",
            "title" to "Build broken",
            "body" to "production is on fire",
            "priority" to "2",
            "receipt_id" to "r" + "A".repeat(29),
            "attachment" to "https://cdn.example/img.png",
        )

        val payload = parseFcmPayload(data)

        assertNotNull(payload)
        payload!!
        assertEquals(42L, payload.id)
        assertEquals(9001L, payload.sendId)
        assertEquals("Build broken", payload.title)
        assertEquals("production is on fire", payload.body)
        assertEquals(2, payload.priority)
        assertEquals("r" + "A".repeat(29), payload.receiptId)
        assertEquals("https://cdn.example/img.png", payload.attachmentUri)
    }

    // 2. `message` is accepted as the body alias (server StoredMessage uses the
    //    `message` JSON tag, mirrored in the WS frame).
    @Test
    fun body_field_alias_message_is_accepted() {
        val payload = parseFcmPayload(mapOf("id" to "7", "message" to "hi"))
        assertNotNull(payload)
        assertEquals("hi", payload!!.body)
        assertNull(payload.title)
    }

    // 3. send_id defaults to id when absent (a self-parented message).
    @Test
    fun send_id_defaults_to_id() {
        val payload = parseFcmPayload(mapOf("id" to "99", "body" to "x"))!!
        assertEquals(99L, payload.sendId)
    }

    // 4. priority defaults to the Pushover normal (0) when absent.
    @Test
    fun priority_defaults_to_zero_when_absent() {
        val payload = parseFcmPayload(mapOf("id" to "1", "body" to "x"))!!
        assertEquals(0, payload.priority)
    }

    // 5. A non-numeric priority falls back to 0 (not treated as corrupt — the
    //    message is still deliverable).
    @Test
    fun non_numeric_priority_falls_back_to_zero() {
        val payload = parseFcmPayload(mapOf("id" to "1", "body" to "x", "priority" to "urgent"))!!
        assertEquals(0, payload.priority)
    }

    // 6. `receipt` is accepted as the receipt_id alias.
    @Test
    fun receipt_field_alias_is_accepted() {
        val payload = parseFcmPayload(mapOf("id" to "1", "body" to "x", "receipt" to "rc1"))!!
        assertEquals("rc1", payload.receiptId)
    }

    // 7. Empty optional strings normalize to null (no zero-length title).
    @Test
    fun empty_optional_strings_become_null() {
        val payload = parseFcmPayload(
            mapOf("id" to "1", "body" to "x", "title" to "", "receipt_id" to "", "attachment" to ""),
        )!!
        assertNull(payload.title)
        assertNull(payload.receiptId)
        assertNull(payload.attachmentUri)
    }

    // ---- corrupt / ignored payloads (failure = ignored, not crash) ----

    // 8. Missing id -> null (cannot attribute a message without a server id).
    @Test
    fun missing_id_is_ignored() {
        assertNull(parseFcmPayload(mapOf("body" to "x")))
    }

    // 9. Non-numeric id -> null.
    @Test
    fun non_numeric_id_is_ignored() {
        assertNull(parseFcmPayload(mapOf("id" to "not-a-number", "body" to "x")))
    }

    // 10. Non-positive id -> null (ids are server high-water marks; <=0 invalid).
    @Test
    fun zero_or_negative_id_is_ignored() {
        assertNull(parseFcmPayload(mapOf("id" to "0", "body" to "x")))
        assertNull(parseFcmPayload(mapOf("id" to "-5", "body" to "x")))
    }

    // 11. Missing body/message -> null.
    @Test
    fun missing_body_is_ignored() {
        assertNull(parseFcmPayload(mapOf("id" to "1")))
    }

    // 12. Empty body and message -> null (no empty notification).
    @Test
    fun empty_body_is_ignored() {
        assertNull(parseFcmPayload(mapOf("id" to "1", "body" to "")))
        assertNull(parseFcmPayload(mapOf("id" to "1", "body" to "", "message" to "")))
    }

    // 13. Empty payload -> null.
    @Test
    fun empty_payload_is_ignored() {
        assertNull(parseFcmPayload(emptyMap()))
    }

    // 14. Junk keys alongside valid ones are ignored (forward-compatible).
    @Test
    fun unknown_keys_are_ignored() {
        val payload = parseFcmPayload(
            mapOf("id" to "3", "body" to "x", "future_field" to "ignored", "sound" to "bugle"),
        )!!
        assertEquals(3L, payload.id)
        assertEquals("x", payload.body)
    }

    // 15. Maps to a MessageEntity-compatible shape: p2 + receipt -> PENDING ack.
    @Test
    fun emergency_payload_maps_to_pending_ack_entity() {
        val payload = parseFcmPayload(
            mapOf("id" to "10", "send_id" to "100", "body" to "wake up", "priority" to "2", "receipt_id" to "rc10"),
        )!!
        val entity = payload.toMessageEntity(sub = "https://srv")
        assertEquals(10L, entity.id)
        assertEquals("https://srv", entity.sub)
        assertEquals(100L, entity.sendId)
        assertEquals("wake up", entity.body)
        assertEquals(2, entity.priority)
        assertEquals("rc10", entity.receiptId)
        // Emergency (receipt present) seeds PENDING so the ack outbox can fire.
        assertEquals(net.pushfree.android.data.AckState.PENDING, entity.ackState)
    }

    // 16. Non-emergency payload maps to NONE ack and null receipt.
    @Test
    fun normal_payload_maps_to_none_ack_entity() {
        val payload = parseFcmPayload(mapOf("id" to "11", "body" to "hi", "priority" to "0"))!!
        val entity = payload.toMessageEntity(sub = "https://srv")
        assertNull(entity.receiptId)
        assertEquals(net.pushfree.android.data.AckState.NONE, entity.ackState)
    }

    // 17. Adversarial: a payload that looks plausible (has keys + a numeric-ish
    //     value) but whose id is corrupt must still be rejected, not counted as
    //     a successful parse from the raw key count.
    @Test
    fun plausible_but_corrupt_id_is_rejected_not_counted() {
        // 5 keys including a "numeric" id that is actually whitespace-padded
        // non-sensical -> must be null despite a non-empty payload map.
        val corrupt = mapOf(
            "id" to " 12 3",
            "body" to "looks real",
            "title" to "real",
            "priority" to "1",
            "receipt_id" to "r1",
        )
        assertEquals(5, corrupt.size) // raw count is misleading...
        assertNull(parseFcmPayload(corrupt)) // ...parse must still reject it.
    }

    // 18. Negative priorities are preserved (Pushover range is -2..2).
    @Test
    fun negative_priority_is_preserved() {
        val payload = parseFcmPayload(mapOf("id" to "1", "body" to "x", "priority" to "-2"))!!
        assertEquals(-2, payload.priority)
    }

    @Test
    fun parses_multiple_valid_payloads_in_sequence() {
        // Repeated parsing is stateless and deterministic (no shared mutable
        // state, no flakiness from prior input).
        for (i in 1..50L) {
            val p = parseFcmPayload(mapOf("id" to i.toString(), "body" to "m$i"))!!
            assertEquals(i, p.id)
            assertEquals("m$i", p.body)
            assertTrue(p.sendId == i)
        }
    }
}
