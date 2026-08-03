package net.pushfree.android.ui.settings

import android.content.Context
import net.pushfree.android.data.AckState
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.notifications.Notifications
import net.pushfree.android.notifications.PushfreeNotificationBuilder

/**
 * Posts a local test notification so the user can confirm channels, sound and
 * the full-screen-intent flow are wired without round-tripping to a server.
 *
 * Kept as an interface so [SettingsViewModel] is unit-testable without the
 * notification subsystem (which needs a real [Context] / Robolectric).
 */
fun interface NotificationTester {
    fun postTestNotification(): TestNotificationResult
}

/** Outcome of a test-notification post, surfaced in the settings UI. */
data class TestNotificationResult(val success: Boolean, val message: String) {
    companion object {
        val OK = TestNotificationResult(true, "Test notification posted")
        val FAILED = TestNotificationResult(false, "Could not post test notification")
    }
}

/**
 * Default tester: builds a default-priority message and hands it to the
 * production notification pipeline (channels are ensured at app start).
 */
class PushfreeNotificationTester(
    private val context: Context,
    private val apiLevel: Int = android.os.Build.VERSION.SDK_INT,
) : NotificationTester {
    override fun postTestNotification(): TestNotificationResult = runCatching {
        Notifications.ensureChannels(context)
        val builder = PushfreeNotificationBuilder(context)
        val msg = MessageEntity(
            id = TEST_ID,
            sub = "test",
            sendId = TEST_ID,
            title = "Pushfree test",
            body = "If you can read this, notifications are configured correctly.",
            priority = 0,
            attachmentUri = null,
            ackState = AckState.NONE,
            receiptId = null,
        )
        val built = builder.build(msg, apiLevel = apiLevel, canUseFullScreenIntent = false)
        val nm = context.getSystemService(android.app.NotificationManager::class.java)
        nm.notify(built.notificationId, built.notification)
        TestNotificationResult.OK
    }.getOrElse { TestNotificationResult.FAILED }

    private companion object {
        const val TEST_ID = 999_001L
    }
}
