package net.pushfree.android.up

import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Pure-JVM unit tests for the [RecordingUpRegistrar] server-call record.
 *
 * This is the lifecycle record the connector keeps: NEW_ENDPOINT records the
 * endpoint (device known to the server over UP); UNREGISTERED clears it (the
 * manual-QA failure scenario: "device removed from server-call record"). The
 * real [OkHttpUpRegistrar] shares the same record transitions plus a best-effort
 * HTTP sync that is intentionally not unit-tested (no network, mirroring
 * [net.pushfree.android.fcm.FcmTokenRegistrar]); these tests pin the observable
 * record contract both implementations must honor.
 */
class UnifiedPushRegistrarTest {

    @Test
    fun starts_unregistered() = runTest {
        val r = RecordingUpRegistrar()
        assertFalse(r.isRegistered())
        assertNull(r.currentEndpoint())
    }

    @Test
    fun new_endpoint_records_the_endpoint_and_registers() = runTest {
        val r = RecordingUpRegistrar()
        assertTrue(r.onNewEndpoint("https://distributor.example/t/abc"))
        assertTrue(r.isRegistered())
        assertEquals("https://distributor.example/t/abc", r.currentEndpoint())
        assertEquals(listOf("register:https://distributor.example/t/abc"), r.calls)
    }

    @Test
    fun unregistered_clears_the_server_call_record() = runTest {
        val r = RecordingUpRegistrar()
        r.onNewEndpoint("https://distributor.example/t/abc")
        assertTrue(r.isRegistered())

        // failure=UNREGISTERED -> device removed from server-call record
        assertTrue(r.onUnregistered())

        assertFalse(r.isRegistered())
        assertNull(r.currentEndpoint())
        assertEquals(
            listOf("register:https://distributor.example/t/abc", "unregister"),
            r.calls,
        )
    }

    @Test
    fun new_endpoint_rotates_the_recorded_endpoint() = runTest {
        val r = RecordingUpRegistrar()
        r.onNewEndpoint("https://distributor.example/t/old")
        r.onNewEndpoint("https://distributor.example/t/new")
        assertEquals("https://distributor.example/t/new", r.currentEndpoint())
        assertTrue(r.isRegistered())
        assertEquals(
            listOf(
                "register:https://distributor.example/t/old",
                "register:https://distributor.example/t/new",
            ),
            r.calls,
        )
    }

    @Test
    fun re_register_after_unregister_records_again() = runTest {
        val r = RecordingUpRegistrar()
        r.onNewEndpoint("https://ep1")
        r.onUnregistered()
        r.onNewEndpoint("https://ep2")
        assertTrue(r.isRegistered())
        assertEquals("https://ep2", r.currentEndpoint())
        assertEquals(listOf("register:https://ep1", "unregister", "register:https://ep2"), r.calls)
    }
}
