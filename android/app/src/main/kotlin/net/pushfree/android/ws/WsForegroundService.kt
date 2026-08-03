package net.pushfree.android.ws

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
import net.pushfree.android.R
import net.pushfree.android.data.AckState
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.data.PushFreeDatabase
import net.pushfree.android.e2ee.E2ee
import net.pushfree.android.e2ee.SharedPrefsE2eeKeyStore
import net.pushfree.android.notifications.Notifications
import net.pushfree.android.notifications.PushfreeNotificationBuilder
import net.pushfree.android.outbox.AckOutboxServices

/**
 * Foreground service that keeps the WebSocket transport alive across doze.
 *
 * Holds a persistent (low-importance) status notification so Android treats the
 * transport as user-visible foreground work, then runs one [WsTransport] per
 * configured server subscription. Incoming messages are persisted to Room and
 * posted through the priority notification pipeline (todo 32); the status
 * notification tracks connection state (ntfy pattern).
 *
 * `START_STICKY` lets Android restart the service under memory pressure; the
 * transport's persisted since-cursor means a restart replays only the unseen
 * tail rather than the whole history.
 */
class WsForegroundService : Service() {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val jobs = mutableListOf<Job>()

    override fun onCreate() {
        super.onCreate()
        ensureStatusChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startForegroundCompat(statusNotification(getString(R.string.ws_status_connecting)))
        scope.launch { relaunchTransports() }
        return START_STICKY
    }

    private suspend fun relaunchTransports() {
        val db = AckOutboxServices.database(applicationContext)
        val subscriptions = runCatching { db.subscriptionDao().getAll() }
            .onFailure { Log.w(TAG, "failed to read subscriptions", it) }
            .getOrDefault(emptyList())

        jobs.forEach { it.cancel() }
        jobs.clear()
        for (sub in subscriptions) {
            val transport = WsTransport(
                client = sharedClient,
                config = WsConfig(sub.serverUrl, sub.deviceId, sub.secret),
                cursor = RoomWsCursorStore(db.sinceCursorDao(), sub.serverUrl),
            )
            jobs += scope.launch { runTransport(transport, sub.serverUrl, db) }
        }
    }

    private suspend fun runTransport(
        transport: WsTransport,
        serverUrl: String,
        db: PushFreeDatabase,
    ) {
        Notifications.ensureChannels(applicationContext)
        val builder = PushfreeNotificationBuilder(applicationContext)
        val nm = getSystemService(NotificationManager::class.java) ?: return
        transport.events().collect { event ->
            when (event) {
                is WsEvent.Connected ->
                    updateStatus(getString(R.string.ws_status_connected))
                is WsEvent.Reconnecting ->
                    updateStatus(getString(R.string.ws_status_reconnecting))
                is WsEvent.Message -> {
                    val m = event.message
                    // Decrypt BEFORE storing (todo 44 ingest hook, todo 28/32
                    // pipeline). A wrong/missing key -> placeholder; the
                    // message is still stored + notified, never the ciphertext.
                    val key = SharedPrefsE2eeKeyStore(applicationContext).get()
                    val (title, body) = E2ee.decryptTitleBody(
                        title = m.title,
                        body = m.body,
                        encrypted = m.encrypted,
                        hexKey = key,
                    )
                    val entity = MessageEntity(
                        id = m.id,
                        sub = serverUrl,
                        sendId = m.sendId,
                        title = title,
                        body = body,
                        priority = m.priority,
                        attachmentUri = m.attachmentUri,
                        ackState = if (m.receiptId != null) AckState.PENDING else AckState.NONE,
                        receiptId = m.receiptId,
                    )
                    db.messageDao().insert(entity)
                    val built = builder.build(entity)
                    runCatching { nm.notify(built.notificationId, built.notification) }
                }
                is WsEvent.Open, WsEvent.Keepalive, is WsEvent.Error -> Unit
            }
        }
    }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    // ---- persistent status notification ----

    private fun ensureStatusChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val nm = getSystemService(NotificationManager::class.java) ?: return
        nm.createNotificationChannel(
            NotificationChannel(
                STATUS_CHANNEL_ID,
                getString(R.string.ws_channel_name),
                NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = getString(R.string.ws_channel_description)
                setShowBadge(false)
            },
        )
    }

    private fun statusNotification(text: String): Notification =
        Notification.Builder(this, STATUS_CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(getString(R.string.notification_default_title))
            .setContentText(text)
            .setOngoing(true)
            .setCategory(Notification.CATEGORY_SERVICE)
            .build()

    private fun startForegroundCompat(notification: Notification) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(
                NOTIFICATION_ID,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC,
            )
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }
    }

    private fun updateStatus(text: String) {
        runCatching {
            getSystemService(NotificationManager::class.java)
                ?.notify(NOTIFICATION_ID, statusNotification(text))
        }
    }

    private companion object {
        const val TAG = "WsForegroundService"
        const val NOTIFICATION_ID = 1
        const val STATUS_CHANNEL_ID = "pf_ws_status"

        // Process-wide OkHttp client; sharing reuses the connection pool +
        // dispatcher across reconnects and across subscriptions.
        val sharedClient: okhttp3.OkHttpClient by lazy { WsClientFactory.build() }
    }
}
