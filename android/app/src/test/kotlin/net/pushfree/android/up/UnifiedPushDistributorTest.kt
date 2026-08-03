package net.pushfree.android.up

import android.content.Context
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/**
 * Robolectric tests for the on-device UnifiedPush distributor availability
 * check (the fallback gate).
 *
 * Spec fallback: if no UP distributor app is installed, the UP transport is
 * gracefully disabled and the WebSocket foreground service stays the primary
 * source. Under Robolectric no third-party app advertises the connector
 * REGISTER action, so [UnifiedPushDistributor.isAvailable] reports false; the
 * register trigger must be a safe no-op in that state (never throws, never
 * blocks onboarding).
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34])
class UnifiedPushDistributorTest {

    private lateinit var context: Context

    @Before
    fun setUp() {
        context = ApplicationProvider.getApplicationContext()
    }

    // No distributor installed -> transport disabled, WS stays primary.
    @Test
    fun is_available_false_when_no_distributor_installed() {
        assertFalse(UnifiedPushDistributor.isAvailable(context))
    }

    // register() is a safe no-op when no distributor is present (fallback path
    // does not crash or broadcast into the void in a way that breaks onboarding).
    @Test
    fun register_is_safe_noop_when_no_distributor() {
        UnifiedPushDistributor.register(context) // must not throw
        UnifiedPushDistributor.unregister(context) // must not throw
        assertFalse(UnifiedPushDistributor.isAvailable(context))
    }

    // Sanity: the connector contract action the availability check probes is the
    // documented REGISTER action (guards against a copy-paste of the wrong
    // action string silently disabling the transport forever).
    @Test
    fun probed_action_is_the_register_action() {
        assertTrue(UnifiedPushContract.ACTION_REGISTER.endsWith(".REGISTER"))
        assertFalse(UnifiedPushContract.ACTION_REGISTER.isEmpty())
    }
}
