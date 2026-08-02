package net.pushfree.android.outbox

import android.app.Notification
import android.app.NotificationManager
import android.content.Context
import androidx.room.Room
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.work.ListenableWorker.Result
import androidx.work.WorkInfo
import androidx.work.WorkManager
import androidx.work.testing.TestListenableWorkerBuilder
import androidx.work.testing.WorkManagerTestInitHelper
import androidx.work.workDataOf
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import net.pushfree.android.data.AckState
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.data.PushFreeDatabase
import net.pushfree.android.data.SubscriptionEntity
import net.pushfree.android.notifications.Notifications
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config
import java.util.UUID

/**
 * Robolectric + WorkManagerTestInitHelper tests for the resilient ack outbox.
 *
 * Covers the three deliverable scenarios — success (acked + notification
 * dismissed), transient failure (requeue via Result.retry), permanent 404
 * (failure + no more retries) — plus offline resilience (enqueued while offline,
 * drained once connectivity returns).
 *
 * All HTTP outcomes are driven through a fake [AckPoster] and an in-memory Room
 * database so the suite is fully deterministic (no real sockets, no timing).
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34])
class AckOutboxTest {

    private lateinit var ctx: Context
    private lateinit var db: PushFreeDatabase
    private val nm: NotificationManager
        get() = ctx.getSystemService(NotificationManager::class.java)

    @Before
    fun setUp() {
        ctx = ApplicationProvider.getApplicationContext()
        WorkManagerTestInitHelper.initializeTestWorkManager(ctx)
        db = Room.inMemoryDatabaseBuilder(ctx, PushFreeDatabase::class.java)
            .allowMainThreadQueries()
            .build()
        AckOutboxServices.setDatabase(db)
        AckOutboxServices.poster = HttpUrlConnectionAckPoster
        Notifications.ensureChannels(ctx)
    }

    @After
    fun tearDown() {
        AckOutboxServices.setDatabase(null)
        AckOutboxServices.poster = HttpUrlConnectionAckPoster
        db.close()
    }

    // 1. Success -> acked + notification dismissed.
    @Test
    fun success_marks_acked_and_dismisses_notification() {
        val notifId = seed(receiptId = "rec-success", msgId = MSG_ID)
        postStubNotification(notifId)
        AckOutboxServices.poster = AckPoster { _, _, _ -> AckPostResult.Success }

        val result = runBlocking { buildWorker("rec-success", MSG_ID, notifId).doWork() }

        assertTrue("expected success, got $result", result is Result.Success)
        runBlocking { assertEquals(AckState.ACKED, db.messageDao().getById(MSG_ID)!!.ackState) }
        assertEquals("notification dismissed on success", 0, activeNotificationCount())
    }

    // 2. Failure -> requeue (Result.retry), message stays pending, notification stays dismissable.
    @Test
    fun transient_failure_requeues_and_keeps_pending() {
        val notifId = seed(receiptId = "rec-retry", msgId = MSG_ID)
        postStubNotification(notifId)
        AckOutboxServices.poster = AckPoster { _, _, _ ->
            AckPostResult.TransientFailure(500, "server error")
        }

        val result = runBlocking { buildWorker("rec-retry", MSG_ID, notifId).doWork() }

        assertTrue("expected retry, got $result", result is Result.Retry)
        runBlocking { assertEquals(AckState.PENDING, db.messageDao().getById(MSG_ID)!!.ackState) }
        assertEquals("notification still dismissable", 1, activeNotificationCount())
    }

    // 3. Permanent 404 -> failure + no more retries, notification left dismissable.
    @Test
    fun permanent_404_fails_and_leaves_notification_dismissable() {
        val notifId = seed(receiptId = "rec-404", msgId = MSG_ID)
        postStubNotification(notifId)
        AckOutboxServices.poster = AckPoster { _, _, _ -> AckPostResult.PermanentFailure(404) }

        val result = runBlocking { buildWorker("rec-404", MSG_ID, notifId).doWork() }

        assertTrue("expected failure, got $result", result is Result.Failure)
        runBlocking { assertEquals(AckState.PENDING, db.messageDao().getById(MSG_ID)!!.ackState) }
        assertEquals("notification still dismissable", 1, activeNotificationCount())
    }

    // 4. Offline resilience: ack queued while offline stays ENQUEUED (awaiting network).
    @Test
    fun offline_ack_is_enqueued_awaiting_network() {
        val notifId = seed(receiptId = "rec-offline", msgId = MSG_ID)
        AckOutboxServices.poster = AckPoster { _, _, _ -> AckPostResult.Success }

        val name = AckOutbox.enqueue(ctx, "rec-offline", MSG_ID, notifId)

        val infos = WorkManager.getInstance(ctx).getWorkInfosForUniqueWork(name).get()
        assertEquals(1, infos.size)
        assertEquals(WorkInfo.State.ENQUEUED, infos[0].state)
        // Constraints unmet -> worker has NOT run -> message still pending.
        runBlocking { assertEquals(AckState.PENDING, db.messageDao().getById(MSG_ID)!!.ackState) }
    }

    // 5. Drain: once the CONNECTED constraint is met, the queued ack runs to success.
    @Test
    fun enqueued_work_drains_when_network_constraint_met() {
        val notifId = seed(receiptId = "rec-drain", msgId = MSG_ID)
        postStubNotification(notifId)
        AckOutboxServices.poster = AckPoster { _, _, _ -> AckPostResult.Success }

        val request = AckOutbox.buildRequest("rec-drain", MSG_ID, notifId)
        val wm = WorkManager.getInstance(ctx)
        wm.enqueue(request).result.get()

        // Awaiting network: ENQUEUED, not yet run.
        assertEquals(WorkInfo.State.ENQUEUED, wm.getWorkInfoById(request.id).get()!!.state)

        // Simulate connectivity restored -> drain.
        WorkManagerTestInitHelper.getTestDriver(ctx)!!.setAllConstraintsMet(request.id)

        val drained = awaitTerminal(wm, request.id)
        assertEquals(WorkInfo.State.SUCCEEDED, drained.state)
        runBlocking { assertEquals(AckState.ACKED, db.messageDao().getById(MSG_ID)!!.ackState) }
        assertEquals("notification dismissed after drain", 0, activeNotificationCount())
    }

    // ---- helpers ----

    private fun buildWorker(receiptId: String, msgId: Long, notifId: Int): AckWorker =
        TestListenableWorkerBuilder.from(ctx, AckWorker::class.java)
            .setInputData(
                workDataOf(
                    AckWorker.KEY_RECEIPT_ID to receiptId,
                    AckWorker.KEY_MESSAGE_ID to msgId,
                    AckWorker.KEY_NOTIFICATION_ID to notifId,
                ),
            )
            .build()

    /** Inserts a PENDING emergency message + its server subscription. Returns the notification id. */
    private fun seed(receiptId: String, msgId: Long): Int {
        runBlocking {
            db.subscriptionDao().upsert(
                SubscriptionEntity(
                    serverUrl = SERVER,
                    userKey = "uk",
                    token = "tk",
                    deviceId = "dev",
                    secret = SECRET,
                ),
            )
            db.messageDao().insert(
                MessageEntity(
                    id = msgId,
                    sub = SERVER,
                    sendId = msgId,
                    title = "t",
                    body = "b",
                    priority = 2,
                    attachmentUri = null,
                    ackState = AckState.PENDING,
                    receiptId = receiptId,
                ),
            )
        }
        return msgId.toInt()
    }

    private fun postStubNotification(id: Int) {
        val n = Notification.Builder(ctx, Notifications.CHANNEL_EMERGENCY)
            .setSmallIcon(android.R.drawable.stat_notify_chat)
            .setContentTitle("stub")
            .setContentText("stub")
            .build()
        nm.notify(id, n)
    }

    private fun activeNotificationCount(): Int = nm.activeNotifications.size

    /** Event-driven wait for a terminal WorkInfo state (no sleeps). */
    private fun awaitTerminal(wm: WorkManager, id: UUID): WorkInfo =
        runBlocking {
            withTimeout(AWAIT_MS) {
                wm.getWorkInfoByIdFlow(id).first { it != null && it.state.isFinished }!!
            }
        }

    private companion object {
        const val SERVER = "https://srv.example"
        const val SECRET = "s3cr3t"
        const val MSG_ID = 77001L
        const val AWAIT_MS = 10_000L
    }
}
