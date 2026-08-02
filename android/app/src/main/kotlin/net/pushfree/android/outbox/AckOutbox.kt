package net.pushfree.android.outbox

import android.content.Context
import androidx.work.BackoffPolicy
import androidx.work.Constraints
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequest
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.workDataOf
import java.util.concurrent.TimeUnit

/**
 * Entry point for the resilient ack outbox.
 *
 * Builds a network-constrained ([NetworkType.CONNECTED]), exponentially-backed-
 * off [AckWorker] keyed by receipt id. Keying with [ExistingWorkPolicy.KEEP]
 * coalesces duplicate ack taps (and re-posted notifications) into a single
 * outstanding job per receipt. While offline the request stays ENQUEUED and
 * drains automatically once connectivity returns.
 */
object AckOutbox {
    const val UNIQUE_WORK_PREFIX = "ack_"

    /** Enqueues an ack for [receiptId]. Returns the unique work name. */
    fun enqueue(
        context: Context,
        receiptId: String,
        messageId: Long = -1L,
        notificationId: Int = -1,
    ): String {
        val request = buildRequest(receiptId, messageId, notificationId)
        val name = uniqueName(receiptId)
        WorkManager.getInstance(context)
            .enqueueUniqueWork(name, ExistingWorkPolicy.KEEP, request)
        return name
    }

    /**
     * Builds the work request. Internal so tests can enqueue directly and drive
     * the WorkManager test driver with the request id in hand.
     */
    internal fun buildRequest(
        receiptId: String,
        messageId: Long,
        notificationId: Int,
    ): OneTimeWorkRequest {
        val constraints = Constraints.Builder()
            .setRequiredNetworkType(NetworkType.CONNECTED)
            .build()
        return OneTimeWorkRequestBuilder<AckWorker>()
            .setConstraints(constraints)
            .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, BACKOFF_SECONDS, TimeUnit.SECONDS)
            .setInputData(
                workDataOf(
                    AckWorker.KEY_RECEIPT_ID to receiptId,
                    AckWorker.KEY_MESSAGE_ID to messageId,
                    AckWorker.KEY_NOTIFICATION_ID to notificationId,
                ),
            )
            .build()
    }

    fun uniqueName(receiptId: String): String = UNIQUE_WORK_PREFIX + receiptId

    private const val BACKOFF_SECONDS = 10L
}
