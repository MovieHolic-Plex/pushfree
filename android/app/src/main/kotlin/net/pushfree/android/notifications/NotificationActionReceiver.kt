package net.pushfree.android.notifications

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationManagerCompat

/**
 * Handles notification action broadcasts produced by
 * [PushfreeNotificationBuilder], specifically the "Acknowledge" button on an
 * emergency-priority message carrying a receipt.
 *
 * Contract (consumed by todo 33's WorkManager outbox):
 *  - Inbound broadcast: [ACTION_ACK] with [EXTRA_RECEIPT_ID] (required),
 *    [EXTRA_MESSAGE_ID], [EXTRA_NOTIFICATION_ID].
 *  - Outbound hand-off: [ACTION_OUTBOX_ACK] (package-local) carrying the same
 *    extras. The outbox enqueues the HTTP `POST /1/receipts/<r>/acknowledge.json`
 *    request and dismisses the notification once the server confirms.
 *
 * This receiver deliberately performs NO network I/O — it only dismisses the
 * notification for immediate responsiveness and forwards the ack to the outbox.
 * The actual HTTP ack and retry logic live in todo 33.
 */
class NotificationActionReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != ACTION_ACK) return
        val receiptId = intent.getStringExtra(EXTRA_RECEIPT_ID) ?: return
        val messageId = intent.getLongExtra(EXTRA_MESSAGE_ID, INVALID_ID)
        val notificationId = intent.getIntExtra(EXTRA_NOTIFICATION_ID, INVALID_ID.toInt())

        // Dismiss immediately for responsiveness; the outbox reconciles state.
        if (notificationId != INVALID_ID.toInt()) {
            runCatching { NotificationManagerCompat.from(context).cancel(notificationId) }
        }

        // Hand off to the ack outbox (todo 33) via a package-local broadcast so
        // this class has no compile-time dependency on WorkManager wiring yet.
        val outbox = Intent(ACTION_OUTBOX_ACK).apply {
            setPackage(context.packageName)
            putExtra(EXTRA_RECEIPT_ID, receiptId)
            if (messageId != INVALID_ID) putExtra(EXTRA_MESSAGE_ID, messageId)
            if (notificationId != INVALID_ID.toInt()) putExtra(EXTRA_NOTIFICATION_ID, notificationId)
        }
        context.sendBroadcast(outbox)
    }

    companion object {
        private const val INVALID_ID = -1L

        /** Inbound: the notification "Acknowledge" action. */
        const val ACTION_ACK = "net.pushfree.android.action.ACK"

        /**
         * Outbound (this receiver -> todo 33 outbox): an ack to enqueue. Carries
         * [EXTRA_RECEIPT_ID], [EXTRA_MESSAGE_ID], [EXTRA_NOTIFICATION_ID].
         */
        const val ACTION_OUTBOX_ACK = "net.pushfree.android.action.OUTBOX_ACK"

        const val EXTRA_RECEIPT_ID = "net.pushfree.android.extra.receipt_id"
        const val EXTRA_MESSAGE_ID = "net.pushfree.android.extra.message_id"
        const val EXTRA_NOTIFICATION_ID = "net.pushfree.android.extra.notification_id"
    }
}
