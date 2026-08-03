package net.pushfree.android.fcm

import android.app.NotificationManager
import android.util.Log
import com.google.firebase.messaging.FirebaseMessagingService
import com.google.firebase.messaging.RemoteMessage
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.e2ee.SharedPrefsE2eeKeyStore
import net.pushfree.android.notifications.Notifications
import net.pushfree.android.notifications.PushfreeNotificationBuilder
import net.pushfree.android.outbox.AckOutboxServices

/**
 * Firebase Cloud Messaging service for the optional FCM transport (todo 30).
 *
 * PLAY FLAVOR ONLY (todo 49): this source set is `src/play`, so the class is
 * compiled exclusively into the `play` variant. The `fdroid` variant has no
 * Firebase dependency on its classpath and never references this type. The FCM
 * `<service>` manifest entry that routes `com.google.firebase.MESSAGING_EVENT`
 * to this class lives in `src/play/AndroidManifest.xml`.
 *
 * Receives data-only messages and persists them as [MessageEntity] rows (Room,
 * todo 28) through the same priority-notification pipeline as the WS transport.
 * Token rotation triggers re-registration to the server so the server-side FCM
 * channel (todo 16) routes to the current registration token (the
 * `devices.fcm_token` column the server reads/clears).
 *
 * This service is only invoked when Firebase is initialized at runtime — i.e.
 * when `google-services.json` was present at build time so the google-services
 * plugin emitted FirebaseOptions. Absent that file the build still succeeds
 * (the plugin is conditionally applied in `app/build.gradle.kts`) and this
 * service is simply never started: no FirebaseApp, no token, no delivery. That
 * is the "runtime-disabled" state required by the spec.
 */
class PushfreeFcmService : FirebaseMessagingService() {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    override fun onMessageReceived(message: RemoteMessage) {
        val data = message.data
        val parsed = parseFcmPayload(data)
        if (parsed == null) {
            // Corrupt / non-pushfree payload: ignore + log. A malformed message
            // must never crash the app or post an empty notification. The raw
            // keyset is logged (sorted, values omitted) so operators can see
            // what arrived without dumping potentially sensitive payloads, and
            // so success is never misreported from a raw count of XML/keys.
            Log.w(TAG, "ignoring malformed FCM payload (keys=${data.keys.sorted()})")
            return
        }
        scope.launch {
            runCatching { persistAndNotify(parsed) }
                .onFailure { Log.w(TAG, "failed to handle FCM message id=${parsed.id}", it) }
        }
    }

    override fun onNewToken(token: String) {
        // Token rotation: re-register so the server targets the current
        // registration token. Failures are retried by the registrar's own
        // backoff and never crash the service.
        scope.launch {
            runCatching { FcmTokenRegistrar(applicationContext).register(token) }
                .onFailure { Log.w(TAG, "FCM token re-registration failed", it) }
        }
    }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }

    private suspend fun persistAndNotify(payload: FcmPayload) {
        val db = AckOutboxServices.database(applicationContext)
        val sub = resolveSub(db) ?: run {
            // No configured subscription yet (onboarding not complete): the
            // message cannot be attributed to a server, so drop + log rather
            // than insert under an orphan key.
            Log.w(TAG, "dropping FCM message id=${payload.id}: no subscription configured")
            return
        }
        val entity = payload.toMessageEntity(
            sub = sub,
            hexKey = SharedPrefsE2eeKeyStore(applicationContext).get(),
        )
        db.messageDao().insert(entity)
        postNotification(entity)
    }

    private suspend fun resolveSub(
        db: net.pushfree.android.data.PushFreeDatabase,
    ): String? {
        // FCM is tied to a single Firebase project -> a single pushfree server.
        // The subscription primary key is the server URL; the first configured
        // subscription is therefore the natural destination. (todo 34 wires the
        // explicit transport/source picker; here we resolve deterministically.)
        val subs = db.subscriptionDao().getAll()
        return subs.firstOrNull()?.serverUrl
    }

    private suspend fun postNotification(entity: MessageEntity) {
        runCatching {
            Notifications.ensureChannels(applicationContext)
            val nm = getSystemService(NotificationManager::class.java) ?: return@runCatching
            val builder = PushfreeNotificationBuilder(applicationContext)
            val built = builder.build(entity)
            nm.notify(built.notificationId, built.notification)
        }.onFailure { Log.w(TAG, "notification post failed id=${entity.id}", it) }
    }

    private companion object {
        const val TAG = "PushfreeFcmService"
    }
}
