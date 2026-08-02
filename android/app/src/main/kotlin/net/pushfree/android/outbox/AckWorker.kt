package net.pushfree.android.outbox

import android.content.Context
import android.util.Log
import androidx.core.app.NotificationManagerCompat
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import net.pushfree.android.data.AckState

/**
 * Resilient acknowledgement worker.
 *
 * On each run it looks up the message + its server subscription, POSTs the ack
 * to `POST {serverUrl}/1/receipts/{receiptId}/acknowledge.json` with the device
 * secret, and reconciles local state:
 *  - Success (HTTP 200 status 1): mark [AckState.ACKED] in Room and dismiss the
 *    notification (id = send_id). The notification is intentionally dismissed
 *    HERE rather than when the user taps, so an ack queued while offline keeps
 *    the emergency visible until the server actually confirms.
 *  - Permanent 404 (receipt gone / GC'd): stop retrying ([Result.failure]) and
 *    log; the notification stays user-dismissable.
 *  - Any other non-2xx or IO error: [Result.retry]; WorkManager re-queues with
 *    exponential backoff under the CONNECTED constraint, so it drains once
 *    connectivity returns.
 *
 * Re-runs are idempotent: if the message is already ACKED the worker succeeds
 * without posting again.
 */
class AckWorker(
    appContext: Context,
    params: WorkerParameters,
) : CoroutineWorker(appContext, params) {

    override suspend fun doWork(): Result {
        val receiptId = inputData.getString(KEY_RECEIPT_ID)
        if (receiptId.isNullOrEmpty()) return Result.failure()
        val messageId = inputData.getLong(KEY_MESSAGE_ID, -1L)
        val notificationId = inputData.getInt(KEY_NOTIFICATION_ID, -1)

        val db = AckOutboxServices.database(applicationContext)
        val message = if (messageId > 0) db.messageDao().getById(messageId) else null
        if (message == null) {
            Log.w(TAG, "ack for $receiptId: message $messageId not found; dropping")
            return Result.failure()
        }
        // Idempotent: a prior successful run already acked this message.
        if (message.ackState == AckState.ACKED) return Result.success()

        val sub = db.subscriptionDao().getByServerUrl(message.sub)
        if (sub == null) {
            Log.w(TAG, "ack for $receiptId: no subscription for ${message.sub}; dropping")
            return Result.failure()
        }

        val targetReceiptId = message.receiptId ?: receiptId
        return when (val res = AckOutboxServices.poster.post(sub.serverUrl, targetReceiptId, sub.secret)) {
            is AckPostResult.Success -> {
                db.messageDao().updateAckState(message.id, AckState.ACKED)
                val notifId = if (notificationId >= 0) notificationId else message.sendId.toInt()
                runCatching { NotificationManagerCompat.from(applicationContext).cancel(notifId) }
                Result.success()
            }
            is AckPostResult.PermanentFailure -> {
                Log.w(TAG, "ack for $receiptId: permanent failure HTTP ${res.httpCode}; stopping retries")
                Result.failure()
            }
            is AckPostResult.TransientFailure -> {
                Log.i(
                    TAG,
                    "ack for $receiptId: transient failure (attempt ${runAttemptCount + 1}) " +
                        "http=${res.httpCode ?: "-"}: ${res.message}; will retry",
                )
                Result.retry()
            }
        }
    }

    companion object {
        const val TAG = "AckOutbox"
        const val KEY_RECEIPT_ID = "receipt_id"
        const val KEY_MESSAGE_ID = "message_id"
        const val KEY_NOTIFICATION_ID = "notification_id"
    }
}
