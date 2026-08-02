package net.pushfree.android.notifications

import android.app.Notification
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.drawable.Icon
import android.os.Build
import net.pushfree.android.MainActivity
import net.pushfree.android.R
import net.pushfree.android.data.MessageEntity

/**
 * Outcome of building a notification. Carries post-time facts (the value to
 * pass to [android.app.NotificationManager.notify], the resolved tier, whether
 * a full-screen intent / fallback vibration was used, etc.) for callers and
 * tests to assert on without re-deriving from the [notification] object.
 */
data class NotificationBuildResult(
    /** Value to pass to NotificationManager.notify(id, ...) — equals the message send_id. */
    val notificationId: Int,
    val notification: Notification,
    val tier: NotificationTier,
    val channelId: String,
    val importance: Int,
    val usedFullScreenIntent: Boolean,
    val usedFallbackVibration: Boolean,
    val attachmentRendered: Boolean,
    /** True when API>=34 emergency could not use FSI; caller should surface settings. */
    val requestFullScreenIntentSettings: Boolean,
)

/**
 * Builds a [Notification] from a [MessageEntity] honoring Pushover priority
 * semantics, the Android 14+ full-screen-intent permission flow, attachment
 * BigPicture rendering (with a safe text-only fallback) and the ack action.
 *
 * [build] never throws: attachment or decode failures degrade to a text-only
 * notification rather than dropping the message.
 */
class PushfreeNotificationBuilder(
    private val context: Context,
    private val attachmentLoader: AttachmentLoader = DefaultAttachmentLoader(context),
    private val smallIconRes: Int = R.drawable.ic_notification,
    private val nowMs: () -> Long = System::currentTimeMillis,
) {
    /** Build using the real device's API level and FSI grant state. */
    fun build(message: MessageEntity): NotificationBuildResult = build(
        message = message,
        apiLevel = Build.VERSION.SDK_INT,
        canUseFullScreenIntent = FullScreenIntentPermission.isGranted(context),
    )

    /**
     * Fully-injectable build for deterministic tests. [apiLevel] and
     * [canUseFullScreenIntent] drive the Android 14+ FSI branch without relying
     * on Robolectric shadow internals.
     */
    fun build(
        message: MessageEntity,
        apiLevel: Int,
        canUseFullScreenIntent: Boolean,
    ): NotificationBuildResult {
        val spec = Notifications.channelSpecFor(message.priority)
        val notificationId = message.sendId.toInt()
        val title = HtmlSanitizer.render(
            message.title?.takeIf { it.isNotBlank() }
                ?: context.getString(R.string.notification_default_title),
        )
        val body = HtmlSanitizer.render(message.body)

        // Attachment -> BigPictureStyle. Any failure -> null -> text-only.
        var attachmentRendered = false
        val bitmap: Bitmap? = message.attachmentUri?.let { uri ->
            runCatching { attachmentLoader.load(uri) }.getOrNull()
        }
        if (bitmap != null) attachmentRendered = true

        val builder = Notification.Builder(context, spec.channelId)
            .setSmallIcon(smallIconRes)
            .setContentTitle(title)
            .setContentText(body)
            .setWhen(nowMs())
            .setShowWhen(true)
            .setAutoCancel(true)
            .setCategory(Notification.CATEGORY_MESSAGE)

        if (bitmap != null) {
            builder.setStyle(
                Notification.BigPictureStyle()
                    .bigPicture(bitmap)
                    .setBigContentTitle(title),
            )
        }

        // Emergency intrusiveness: use the full-screen intent when permitted —
        // unconditionally below API 34, and on API 34+ only when the system
        // grants USE_FULL_SCREEN_INTENT. Otherwise fall back to heads-up +
        // strong vibration. The emergency channel (IMPORTANCE_MAX) is always
        // used, so an emergency notification is NEVER silent.
        val useFullScreenIntent = spec.wantsFullScreenIntent &&
            (apiLevel < Build.VERSION_CODES.UPSIDE_DOWN_CAKE || canUseFullScreenIntent)
        var usedFallbackVibration = false
        if (spec.tier == NotificationTier.EMERGENCY) {
            if (useFullScreenIntent) {
                builder.setFullScreenIntent(launchPendingIntent(message), true)
            } else {
                usedFallbackVibration = true
                builder.setPriority(Notification.PRIORITY_MAX)
                builder.setVibrate(Notifications.VIBRATION_PATTERN_EMERGENCY)
                builder.setCategory(Notification.CATEGORY_ALARM)
            }
        }

        // Acknowledge action: present for emergency messages carrying a receipt.
        // Wired to [NotificationActionReceiver]; the HTTP ack POST is todo 33's
        // responsibility — this builder only defines the intent contract.
        if (!message.receiptId.isNullOrEmpty()) {
            builder.addAction(
                Notification.Action.Builder(
                    Icon.createWithResource(context, smallIconRes),
                    context.getString(R.string.notification_action_ack),
                    ackPendingIntent(message, notificationId),
                ).build(),
            )
        }

        val notification = builder.build()
        val requestFsiSettings = spec.tier == NotificationTier.EMERGENCY &&
            apiLevel >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE &&
            !canUseFullScreenIntent

        return NotificationBuildResult(
            notificationId = notificationId,
            notification = notification,
            tier = spec.tier,
            channelId = spec.channelId,
            importance = spec.importance,
            usedFullScreenIntent = useFullScreenIntent,
            usedFallbackVibration = usedFallbackVibration,
            attachmentRendered = attachmentRendered,
            requestFullScreenIntentSettings = requestFsiSettings,
        )
    }

    /** PendingIntent that launches the app (full-screen touch target). */
    private fun launchPendingIntent(message: MessageEntity): PendingIntent {
        val intent = Intent(context, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TASK
            putExtra(NotificationActionReceiver.EXTRA_MESSAGE_ID, message.id)
        }
        return PendingIntent.getActivity(
            context,
            message.id.toInt(),
            intent,
            PendingIntent.FLAG_IMMUTABLE,
        )
    }

    /** Broadcast PendingIntent that requests an ack for an emergency receipt. */
    private fun ackPendingIntent(message: MessageEntity, notificationId: Int): PendingIntent {
        val intent = Intent(context, NotificationActionReceiver::class.java).apply {
            action = NotificationActionReceiver.ACTION_ACK
            putExtra(NotificationActionReceiver.EXTRA_RECEIPT_ID, message.receiptId)
            putExtra(NotificationActionReceiver.EXTRA_MESSAGE_ID, message.id)
            putExtra(NotificationActionReceiver.EXTRA_NOTIFICATION_ID, notificationId)
        }
        return PendingIntent.getBroadcast(
            context,
            message.id.toInt(),
            intent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
    }
}
