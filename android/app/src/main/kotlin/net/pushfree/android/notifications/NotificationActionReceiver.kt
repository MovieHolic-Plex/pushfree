package net.pushfree.android.notifications

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import net.pushfree.android.outbox.AckOutbox

/**
 * Handles notification action broadcasts produced by
 * [PushfreeNotificationBuilder], specifically the "Acknowledge" button on an
 * emergency-priority message carrying a receipt.
 *
 * Contract:
 *  - Inbound broadcast: [ACTION_ACK] with [EXTRA_RECEIPT_ID] (required),
 *    [EXTRA_MESSAGE_ID], [EXTRA_NOTIFICATION_ID].
 *  - Outbound hand-off: enqueues a resilient [AckWorker] via [AckOutbox] carrying
 *    receipt_id + send/message id + notification id.
 *
 * This receiver performs NO network I/O. It does NOT dismiss the notification
 * here: the notification stays visible until the server confirms the ack (HTTP
 * 200 status 1), at which point [AckWorker] dismisses it. This keeps an offline
 * ack's emergency visible until it actually drains. Duplicate taps are coalesced
 * by the outbox's per-receipt unique-work key.
 */
class NotificationActionReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != ACTION_ACK) return
        val receiptId = intent.getStringExtra(EXTRA_RECEIPT_ID) ?: return
        val messageId = intent.getLongExtra(EXTRA_MESSAGE_ID, INVALID_ID)
        val notificationId = intent.getIntExtra(EXTRA_NOTIFICATION_ID, INVALID_ID.toInt())

        AckOutbox.enqueue(
            context = context,
            receiptId = receiptId,
            messageId = if (messageId != INVALID_ID) messageId else -1L,
            notificationId = if (notificationId != INVALID_ID.toInt()) notificationId else -1,
        )
    }

    companion object {
        private const val INVALID_ID = -1L

        /** Inbound: the notification "Acknowledge" action. */
        const val ACTION_ACK = "net.pushfree.android.action.ACK"

        const val EXTRA_RECEIPT_ID = "net.pushfree.android.extra.receipt_id"
        const val EXTRA_MESSAGE_ID = "net.pushfree.android.extra.message_id"
        const val EXTRA_NOTIFICATION_ID = "net.pushfree.android.extra.notification_id"
    }
}
