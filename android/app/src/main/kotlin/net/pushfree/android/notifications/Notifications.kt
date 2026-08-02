package net.pushfree.android.notifications

import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Context
import android.os.Build

/**
 * Pushover message priority -> Android notification channel mapping.
 *
 * Four tiers, matching the deliverable matrix:
 *  - priority <= -1  -> SILENT    (quiet, no sound, IMPORTANCE_LOW)
 *  - priority ==  0  -> DEFAULT   (IMPORTANCE_DEFAULT)
 *  - priority ==  1  -> HIGH      (heads-up, IMPORTANCE_HIGH)
 *  - priority >=  2  -> EMERGENCY (IMPORTANCE_MAX + full-screen intent + vibration)
 *
 * Note: the plan maps `silent` to priority -1 (NOT -2). Values below -1 clamp
 * into SILENT; values above 2 clamp into EMERGENCY.
 */
enum class NotificationTier {
    SILENT,
    DEFAULT,
    HIGH,
    EMERGENCY,
}

/**
 * Pure description of a notification channel derived from a Pushover priority.
 * Holds no Android framework state, so it is trivially unit-testable.
 */
data class ChannelSpec(
    val tier: NotificationTier,
    val channelId: String,
    val channelName: String,
    val importance: Int,
    val vibrate: Boolean,
    val wantsFullScreenIntent: Boolean,
)

object Notifications {
    const val CHANNEL_SILENT = "pf_priority_silent"
    const val CHANNEL_DEFAULT = "pf_priority_default"
    const val CHANNEL_HIGH = "pf_priority_high"
    const val CHANNEL_EMERGENCY = "pf_priority_emergency"

    /**
     * Strong vibration pattern used by the emergency channel and by the
     * full-screen-intent fallback (API 34+ when the FSI permission is denied).
     */
    val VIBRATION_PATTERN_EMERGENCY = longArrayOf(0, 500, 250, 500, 250, 1000)

    /**
     * Pure mapping from a Pushover priority (-2..2) to a [ChannelSpec].
     * Out-of-range values clamp to the nearest tier so the result is always
     * well-defined.
     */
    fun channelSpecFor(priority: Int): ChannelSpec = when {
        priority <= -1 -> ChannelSpec(
            tier = NotificationTier.SILENT,
            channelId = CHANNEL_SILENT,
            channelName = "Silent",
            importance = NotificationManager.IMPORTANCE_LOW,
            vibrate = false,
            wantsFullScreenIntent = false,
        )
        priority == 0 -> ChannelSpec(
            tier = NotificationTier.DEFAULT,
            channelId = CHANNEL_DEFAULT,
            channelName = "Default",
            importance = NotificationManager.IMPORTANCE_DEFAULT,
            vibrate = false,
            wantsFullScreenIntent = false,
        )
        priority == 1 -> ChannelSpec(
            tier = NotificationTier.HIGH,
            channelId = CHANNEL_HIGH,
            channelName = "High",
            importance = NotificationManager.IMPORTANCE_HIGH,
            vibrate = false,
            wantsFullScreenIntent = false,
        )
        else -> ChannelSpec(
            tier = NotificationTier.EMERGENCY,
            channelId = CHANNEL_EMERGENCY,
            channelName = "Emergency",
            importance = NotificationManager.IMPORTANCE_MAX,
            vibrate = true,
            wantsFullScreenIntent = true,
        )
    }

    /**
     * Create the four priority channels. Safe to call repeatedly (creating a
     * channel with an existing id is a no-op). Callers that post notifications
     * (FCM/UnifiedPush handlers, the WorkManager outbox in todo 33) must invoke
     * this before [android.app.NotificationManager.notify].
     */
    fun ensureChannels(context: Context) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val nm = context.getSystemService(NotificationManager::class.java) ?: return

        fun channel(spec: ChannelSpec) = NotificationChannel(
            spec.channelId, spec.channelName, spec.importance,
        ).apply {
            description = "Pushfree ${spec.channelName} priority notifications"
            enableVibration(spec.vibrate)
            // Silent tier makes no sound; every other tier uses the default sound.
            if (!spec.vibrate && spec.tier == NotificationTier.SILENT) {
                setSound(null, null)
            }
        }

        nm.createNotificationChannel(channel(channelSpecFor(-1)))
        nm.createNotificationChannel(channel(channelSpecFor(0)))
        nm.createNotificationChannel(channel(channelSpecFor(1)))
        nm.createNotificationChannel(channel(channelSpecFor(2)).apply {
            vibrationPattern = VIBRATION_PATTERN_EMERGENCY
        })
    }
}
