package net.pushfree.android.up

import net.pushfree.android.data.AckState
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Pure-JVM unit tests for the UnifiedPush message payload parser. No Android
 * framework types, no Robolectric, no network — `parseUpMessage` is a pure
 * function on a `ByteArray` (the raw POST body the server sent to the
 * distributor endpoint), and `org.json.JSONObject` is provided by the local
 * unit-test classpath.
 *
 * Mirrors [net.pushfree.android.fcm.FcmPayloadTest] field-for-field so the two
 * transports stay contract-identical. Covers the acceptance: happy = a complete
 * payload parses to the expected fields; failure = a corrupt payload is ignored
 * (returns null) rather than crashing or yielding a partial entity.
 */
class UnifiedPushMessageTest {

    private fun json(s: String): ByteArray = s.toByteArray(Charsets.UTF_8)

    // 1. Happy path: a complete, well-formed payload round-trips every field.
    @Test
    fun parses_complete_payload() {
        val payload = parseUpMessage(
            json(
                """{"id":42,"send_id":9001,"title":"Build broken","body":"prod on fire",
                   "priority":2,"receipt_id":"rAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","attachment":"https://cdn/x.png"}""",
            ),
        )
        assertEquals(42L, payload!!.id)
        assertEquals(9001L, payload.sendId)
        assertEquals("Build broken", payload.title)
        assertEquals("prod on fire", payload.body)
        assertEquals(2, payload.priority)
        assertEquals("rAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", payload.receiptId)
        assertEquals("https://cdn/x.png", payload.attachmentUri)
    }

    // 2. `message` is accepted as the body alias (WS frame / server Message tag).
    @Test
    fun body_field_alias_message_is_accepted() {
        val payload = parseUpMessage(json("""{"id":7,"message":"hi"}"""))!!
        assertEquals("hi", payload.body)
        assertNull(payload.title)
    }

    // 3. send_id defaults to id when absent (a self-parented message).
    @Test
    fun send_id_defaults_to_id() {
        val payload = parseUpMessage(json("""{"id":99,"body":"x"}"""))!!
        assertEquals(99L, payload.sendId)
    }

    // 4. priority defaults to the Pushover normal (0) when absent.
    @Test
    fun priority_defaults_to_zero_when_absent() {
        val payload = parseUpMessage(json("""{"id":1,"body":"x"}"""))!!
        assertEquals(0, payload.priority)
    }

    // 5. A non-numeric priority falls back to 0 (deliverable, not corrupt).
    @Test
    fun non_numeric_priority_falls_back_to_zero() {
        val payload = parseUpMessage(json("""{"id":1,"body":"x","priority":"urgent"}"""))!!
        assertEquals(0, payload.priority)
    }

    // 6. `receipt` is accepted as the receipt_id alias.
    @Test
    fun receipt_field_alias_is_accepted() {
        val payload = parseUpMessage(json("""{"id":1,"body":"x","receipt":"rc1"}"""))!!
        assertEquals("rc1", payload.receiptId)
    }

    // 7. Empty optional strings normalize to null.
    @Test
    fun empty_optional_strings_become_null() {
        val payload = parseUpMessage(
            json("""{"id":1,"body":"x","title":"","receipt_id":"","attachment":""}"""),
        )!!
        assertNull(payload.title)
        assertNull(payload.receiptId)
        assertNull(payload.attachmentUri)
    }

    // ---- corrupt / ignored payloads (failure = ignored, not crash) ----

    // 8. Missing id -> null.
    @Test
    fun missing_id_is_ignored() {
        assertNull(parseUpMessage(json("""{"body":"x"}""")))
    }

    // 9. Non-positive id -> null (ids are server high-water marks).
    @Test
    fun zero_or_negative_id_is_ignored() {
        assertNull(parseUpMessage(json("""{"id":0,"body":"x"}""")))
        assertNull(parseUpMessage(json("""{"id":-5,"body":"x"}""")))
    }

    // 10. Missing body/message -> null.
    @Test
    fun missing_body_is_ignored() {
        assertNull(parseUpMessage(json("""{"id":1}""")))
    }

    // 11. Empty body and message -> null.
    @Test
    fun empty_body_is_ignored() {
        assertNull(parseUpMessage(json("""{"id":1,"body":""}""")))
        assertNull(parseUpMessage(json("""{"id":1,"body":"","message":""}""")))
    }

    // 12. Empty payload -> null.
    @Test
    fun empty_payload_is_ignored() {
        assertNull(parseUpMessage(json("")))
    }

    // 13. Invalid JSON -> null.
    @Test
    fun invalid_json_is_ignored() {
        assertNull(parseUpMessage(json("not-json-at-all")))
        assertNull(parseUpMessage(json("""{"id":1,"body":""")))
    }

    // 14. Junk keys alongside valid ones are ignored (forward-compatible).
    @Test
    fun unknown_keys_are_ignored() {
        val payload = parseUpMessage(
            json("""{"id":3,"body":"x","future_field":"ignored","sound":"bugle"}"""),
        )!!
        assertEquals(3L, payload.id)
        assertEquals("x", payload.body)
    }

    // 15. Maps to a MessageEntity: p2 + receipt -> PENDING ack.
    @Test
    fun emergency_payload_maps_to_pending_ack_entity() {
        val payload = parseUpMessage(
            json("""{"id":10,"send_id":100,"body":"wake up","priority":2,"receipt_id":"rc10"}"""),
        )!!
        val entity = payload.toMessageEntity(sub = "https://srv")
        assertEquals(10L, entity.id)
        assertEquals("https://srv", entity.sub)
        assertEquals(100L, entity.sendId)
        assertEquals(2, entity.priority)
        assertEquals(AckState.PENDING, entity.ackState)
    }

    // 16. Non-emergency payload maps to NONE ack and null receipt.
    @Test
    fun normal_payload_maps_to_none_ack_entity() {
        val payload = parseUpMessage(json("""{"id":11,"body":"hi","priority":0}"""))!!
        val entity = payload.toMessageEntity(sub = "https://srv")
        assertNull(entity.receiptId)
        assertEquals(AckState.NONE, entity.ackState)
    }

    // 17. Adversarial: a payload with a numeric-looking but corrupt id must be
    //     rejected, not counted as a successful parse from raw byte length.
    @Test
    fun plausible_but_corrupt_id_is_rejected_not_counted() {
        val raw = """{"id":" 12 3","body":"looks real","title":"real","priority":1}"""
        assertTrue(raw.toByteArray().isNotEmpty()) // raw presence is misleading...
        assertNull(parseUpMessage(json(raw)))      // ...parse must still reject it.
    }

    // 18. Negative priorities are preserved (Pushover range is -2..2).
    @Test
    fun negative_priority_is_preserved() {
        val payload = parseUpMessage(json("""{"id":1,"body":"x","priority":-2}"""))!!
        assertEquals(-2, payload.priority)
    }

    // 19. Repeated parsing is stateless and deterministic.
    @Test
    fun parses_multiple_valid_payloads_in_sequence() {
        for (i in 1..50L) {
            val p = parseUpMessage(json("""{"id":$i,"body":"m$i"}"""))!!
            assertEquals(i, p.id)
            assertEquals("m$i", p.body)
            assertTrue(p.sendId == i)
        }
    }

    // 20. UTF-8 multibyte content round-trips (server counts runes, not bytes).
    @Test
    fun utf8_multibyte_body_round_trips() {
        val payload = parseUpMessage(json("""{"id":1,"body":"こんにちは"}"""))!!
        assertEquals("こんにちは", payload.body)
    }
}
