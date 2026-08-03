package net.pushfree.android.up

import android.util.Log
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.data.PushFreeDatabase

/**
 * Maps a decoded [UpIntent] onto side effects: a received message is persisted
 * as a [MessageEntity] (Room, todo 28) through the same store every transport
 * uses, and the NEW_ENDPOINT / UNREGISTERED lifecycle events drive the
 * [UpRegistrar] (the server-call record).
 *
 * This is the testable core of the UnifiedPush connector: the receiver
 * ([PushfreeUpReceiver]) only extracts intent extras and feeds them here, so
 * the dispatch logic is exercisable with an in-memory Room database and a
 * [RecordingUpRegistrar] — no emulator, no network.
 *
 * @param database the pushfree Room store (message + subscription DAOs).
 * @param registrar the UP device<->server registration record.
 * @param onMessagePersisted invoked after a received message is stored, so the
 *        receiver can post the priority notification (todo 32). Default no-op;
 *        tests inject a recorder or leave it empty.
 */
class UpDispatcher(
    private val database: PushFreeDatabase,
    private val registrar: UpRegistrar,
    private val onMessagePersisted: suspend (MessageEntity) -> Unit = {},
    /** Supplies the configured E2EE key (todo 44), or null when unset. */
    private val keyProvider: () -> String? = { null },
) {
    /**
     * Apply [event]. A MESSAGE_RECEIVED whose subscription is not yet
     * configured (onboarding incomplete) is dropped + logged rather than
     * inserted under an orphan key (mirrors the FCM service behavior).
     */
    suspend fun dispatch(event: UpIntent) {
        when (event) {
            is UpIntent.MessageReceived -> persistMessage(event.message)
            is UpIntent.NewEndpoint -> registrar.onNewEndpoint(event.endpoint)
            is UpIntent.Unregistered -> registrar.onUnregistered()
            is UpIntent.MessageIgnored,
            is UpIntent.Registered,
            is UpIntent.Unknown -> Unit
        }
    }

    private suspend fun persistMessage(message: UpMessage) {
        val sub = database.subscriptionDao().getAll().firstOrNull()?.serverUrl ?: run {
            Log.w(TAG, "dropping UP message id=${message.id}: no subscription configured")
            return
        }
        val entity = message.toMessageEntity(sub = sub, hexKey = keyProvider())
        database.messageDao().insert(entity)
        onMessagePersisted(entity)
    }

    private companion object {
        const val TAG = "UpDispatcher"
    }
}
