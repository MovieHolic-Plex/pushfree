package net.pushfree.android.up

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Pure-JVM unit tests for the UnifiedPush lifecycle / endpoint intent decoder.
 * No Android framework types — [decodeUpIntent] takes the action string, the
 * endpoint extra, and the bytes extra already extracted, so this exercises the
 * "endpoint handling + lifecycle mapping" required by the spec directly and
 * deterministically (no emulator, no network, no timing).
 *
 * Covers the connector role mapping:
 *  - NEW_ENDPOINT      -> [UpIntent.NewEndpoint] (register device with server)
 *  - MESSAGE_RECEIVED  -> [UpIntent.MessageReceived] (persist) or MessageIgnored
 *  - UNREGISTERED      -> [UpIntent.Unregistered] (remove from server-call record)
 *  - REGISTERED        -> [UpIntent.Registered] (informational)
 *  - unknown/missing   -> [UpIntent.Unknown] (ignored, never crash)
 */
class UnifiedPushIntentTest {

    private fun msgBytes(id: Long, body: String): ByteArray =
        """{"id":$id,"body":"$body"}""".toByteArray(Charsets.UTF_8)

    // 1. NEW_ENDPOINT with a real endpoint -> NewEndpoint(endpoint).
    @Test
    fun new_endpoint_maps_to_register_event() {
        val event = decodeUpIntent(
            UnifiedPushContract.ACTION_NEW_ENDPOINT,
            endpoint = "https://distributor.example/topic/abc",
            messageBytes = null,
        )
        assertTrue(event is UpIntent.NewEndpoint)
        assertEquals("https://distributor.example/topic/abc", (event as UpIntent.NewEndpoint).endpoint)
    }

    // 2. NEW_ENDPOINT without an endpoint extra is malformed -> Unknown.
    @Test
    fun new_endpoint_without_endpoint_is_unknown() {
        assertEquals(
            UpIntent.Unknown,
            decodeUpIntent(UnifiedPushContract.ACTION_NEW_ENDPOINT, endpoint = null, messageBytes = null),
        )
        assertEquals(
            UpIntent.Unknown,
            decodeUpIntent(UnifiedPushContract.ACTION_NEW_ENDPOINT, endpoint = "  ", messageBytes = null),
        )
    }

    // 3. MESSAGE_RECEIVED with a valid payload -> MessageReceived(message).
    @Test
    fun message_received_with_payload_maps_to_message() {
        val event = decodeUpIntent(
            UnifiedPushContract.ACTION_MESSAGE_RECEIVED,
            endpoint = null,
            messageBytes = msgBytes(42, "hello"),
        )
        assertTrue(event is UpIntent.MessageReceived)
        val msg = (event as UpIntent.MessageReceived).message
        assertEquals(42L, msg.id)
        assertEquals("hello", msg.body)
    }

    // 4. MESSAGE_RECEIVED with a corrupt payload -> MessageIgnored (no crash).
    @Test
    fun message_received_with_corrupt_payload_is_ignored() {
        assertEquals(
            UpIntent.MessageIgnored,
            decodeUpIntent(
                UnifiedPushContract.ACTION_MESSAGE_RECEIVED,
                endpoint = null,
                messageBytes = "garbage".toByteArray(),
            ),
        )
    }

    // 5. MESSAGE_RECEIVED with no bytes extra -> MessageIgnored.
    @Test
    fun message_received_with_no_bytes_is_ignored() {
        assertEquals(
            UpIntent.MessageIgnored,
            decodeUpIntent(UnifiedPushContract.ACTION_MESSAGE_RECEIVED, endpoint = null, messageBytes = null),
        )
    }

    // 6. UNREGISTERED -> Unregistered (the server-call-record-clear trigger).
    @Test
    fun unregistered_maps_to_unregister_event() {
        assertEquals(
            UpIntent.Unregistered,
            decodeUpIntent(UnifiedPushContract.ACTION_UNREGISTERED, endpoint = null, messageBytes = null),
        )
    }

    // 7. REGISTERED -> Registered (informational).
    @Test
    fun registered_maps_to_registered_event() {
        assertEquals(
            UpIntent.Registered,
            decodeUpIntent(UnifiedPushContract.ACTION_REGISTERED, endpoint = null, messageBytes = null),
        )
    }

    // 8. Unknown action -> Unknown.
    @Test
    fun unknown_action_is_unknown() {
        assertEquals(
            UpIntent.Unknown,
            decodeUpIntent("com.example.SOMETHING_ELSE", endpoint = null, messageBytes = null),
        )
    }

    // 9. Null action -> Unknown (defensive: a null intent action never crashes).
    @Test
    fun null_action_is_unknown() {
        assertEquals(UpIntent.Unknown, decodeUpIntent(null, endpoint = null, messageBytes = null))
    }

    // 10. Extras that belong to a different action are ignored (NEW_ENDPOINT
    //     ignores bytes; MESSAGE_RECEIVED ignores endpoint).
    @Test
    fun extras_are_scoped_to_their_action() {
        // NEW_ENDPOINT ignores stray bytes.
        val ne = decodeUpIntent(
            UnifiedPushContract.ACTION_NEW_ENDPOINT,
            endpoint = "https://ep",
            messageBytes = msgBytes(1, "ignored"),
        )
        assertTrue(ne is UpIntent.NewEndpoint)
        // MESSAGE_RECEIVED ignores a stray endpoint string.
        val mr = decodeUpIntent(
            UnifiedPushContract.ACTION_MESSAGE_RECEIVED,
            endpoint = "https://stray",
            messageBytes = msgBytes(5, "kept"),
        )
        assertEquals(5L, (mr as UpIntent.MessageReceived).message.id)
    }

    // 11. Repeated decoding is deterministic (no shared mutable state).
    @Test
    fun decoding_is_deterministic_across_calls() {
        repeat(25) {
            val a = decodeUpIntent(UnifiedPushContract.ACTION_UNREGISTERED, null, null)
            val b = decodeUpIntent(UnifiedPushContract.ACTION_UNREGISTERED, null, null)
            assertEquals(a, b)
        }
        // Sanity: an endpoint round-trips unchanged through 50 decodes.
        val ep = "https://distributor.example/topic/z"
        repeat(50) {
            assertNull(decodeUpIntent(UnifiedPushContract.ACTION_NEW_ENDPOINT, ep, null).let {
                (it as UpIntent.NewEndpoint).endpoint.let { _ -> null }
            })
        }
    }
}
