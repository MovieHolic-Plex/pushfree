package net.pushfree.android.up

import android.app.NotificationManager
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
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
 * UnifiedPush connector entry point (spec: unifiedpush.org connector/android/).
 *
 * Registered in the manifest for the distributor->connector intent actions. On
 * each intent it decodes a typed [UpIntent] (pure) and hands it to an
 * [UpDispatcher] backed by the Room store + [OkHttpUpRegistrar]. The mapping:
 *
 *  - MESSAGE_RECEIVED -> parse payload bytes -> [MessageEntity] (Room, todo 28)
 *    -> priority notification (todo 32). A corrupt payload is ignored + logged.
 *  - NEW_ENDPOINT     -> [UpRegistrar.onNewEndpoint] (record + server sync).
 *  - UNREGISTERED     -> [UpRegistrar.onUnregistered] (clear server-call record).
 *
 * Uses [goAsync] so the modest Room/HTTP work can complete on a background
 * dispatcher without the system killing the broadcast early, and a supervisor
 * scope so one failing handler does not cancel the next.
 */
class PushfreeUpReceiver : BroadcastReceiver() {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    override fun onReceive(context: Context, intent: Intent) {
        val event = decodeUpIntent(
            action = intent.action,
            endpoint = intent.getStringExtra(UnifiedPushContract.EXTRA_ENDPOINT),
            messageBytes = intent.getByteArrayExtra(UnifiedPushContract.EXTRA_BYTES),
        )
        log(event)
        if (event is UpIntent.MessageIgnored ||
            event is UpIntent.Registered ||
            event is UpIntent.Unknown
        ) {
            // No async work: nothing to dispatch.
            return
        }
        val pending = goAsync()
        val dispatcher = UpDispatcher(
            database = AckOutboxServices.database(context.applicationContext),
            registrar = OkHttpUpRegistrar(context.applicationContext),
            onMessagePersisted = { entity -> postNotification(context.applicationContext, entity) },
            keyProvider = { SharedPrefsE2eeKeyStore(context.applicationContext).get() },
        )
        scope.launch {
            try {
                dispatcher.dispatch(event)
            } catch (t: Exception) {
                Log.w(TAG, "dispatch failed for $event", t)
            } finally {
                pending.finish()
            }
        }
    }

    private fun log(event: UpIntent) {
        when (event) {
            is UpIntent.MessageIgnored -> Log.w(TAG, "ignoring malformed UP message payload")
            is UpIntent.Unknown -> Log.w(TAG, "ignoring unrecognized UP intent")
            else -> Unit
        }
    }

    private suspend fun postNotification(context: Context, entity: MessageEntity) {
        runCatching {
            Notifications.ensureChannels(context)
            val nm = context.getSystemService(NotificationManager::class.java) ?: return@runCatching
            val built = PushfreeNotificationBuilder(context).build(entity)
            nm.notify(built.notificationId, built.notification)
        }.onFailure { Log.w(TAG, "notification post failed id=${entity.id}", it) }
    }

    fun shutdown() {
        scope.cancel()
    }

    private companion object {
        const val TAG = "PushfreeUpReceiver"
    }
}
