package net.pushfree.android.up

import net.pushfree.android.data.AckState
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.e2ee.E2ee
import org.json.JSONObject

/**
 * A parsed pushfree push delivered over UnifiedPush. Field keys mirror the
 * `/1/ws` frame ([net.pushfree.android.ws.WsProtocol]) and the FCM data payload
 * (todo 30) so a message looks identical regardless of transport:
 *
 * - `id`            server message id (required, must be a positive long)
 * - `send_id`       parent send id; defaults to `id` when absent
 * - `title`         optional title
 * - `body` / `message`   message text (required; the WS frame uses `message`)
 * - `priority`      Pushover priority -2..2; defaults to 0 when absent/unparseable
 * - `receipt_id` / `receipt`  receipt id for p2 messages (optional)
 * - `attachment`    attachment URI (optional)
 *
 * The UnifiedPush distributor delivers the bytes the server POSTed to the
 * distributor endpoint unchanged, so the connector parses the same JSON shape
 * the server's `/up/{sub}/messages.json` (todo 17) and the WS frame emit.
 */
data class UpMessage(
    val id: Long,
    val sendId: Long,
    val title: String?,
    val body: String,
    val priority: Int,
    val receiptId: String?,
    val attachmentUri: String?,
    /** True when title/body are E2EE base64 blobs (todo 44). */
    val encrypted: Boolean = false,
)

/**
 * Parse a UnifiedPush message payload ([bytes] = the raw POST body the server
 * sent to the distributor endpoint) into an [UpMessage].
 *
 * Returns null for a corrupt payload: non-UTF-8 bytes, invalid JSON, a
 * missing/non-positive `id`, or no `body`/`message`. Callers
 * ([PushfreeUpReceiver]) treat null as "ignore + log" so a bad push never
 * crashes the app or posts a bogus notification. This mirrors
 * [net.pushfree.android.fcm.parseFcmPayload] exactly for transport parity and
 * is a pure function (no Android types) — it runs as a plain JVM unit test
 * (`org.json.JSONObject` is provided by the local unit-test classpath).
 */
fun parseUpMessage(bytes: ByteArray): UpMessage? {
    val text = try {
        String(bytes, Charsets.UTF_8)
    } catch (_: Exception) {
        return null
    }
    val json = try {
        JSONObject(text)
    } catch (_: Exception) {
        return null
    }
    val id = json.optLong("id", 0L)
    if (id <= 0L) return null
    val body = json.optString("body").takeIf { it.isNotEmpty() }
        ?: json.optString("message").takeIf { it.isNotEmpty() }
        ?: return null
    val sendId = json.optLong("send_id", id)
    val title = json.optString("title").takeIf { it.isNotEmpty() }
    // A non-numeric priority falls back to the Pushover default (0); it is not
    // treated as corrupt because the message is still deliverable.
    val priority = json.optInt("priority", 0)
    val receiptId = json.optString("receipt_id").takeIf { it.isNotEmpty() }
        ?: json.optString("receipt").takeIf { it.isNotEmpty() }
    val attachmentUri = json.optString("attachment").takeIf { it.isNotEmpty() }
    val encrypted = json.booleanish("encrypted", false)
    return UpMessage(
        id = id,
        sendId = sendId,
        title = title,
        body = body,
        priority = priority,
        receiptId = receiptId,
        attachmentUri = attachmentUri,
        encrypted = encrypted,
    )
}

/**
 * Map a parsed [UpMessage] onto a Room [MessageEntity] attributed to [sub].
 * When [hexKey] is supplied and [UpMessage.encrypted] is true, the title/body
 * are decrypted before storage (todo 44); any failure yields a placeholder so
 * ciphertext never reaches the UI. `hexKey` defaults to null so existing
 * non-encrypted tests compile unchanged.
 */
internal fun UpMessage.toMessageEntity(sub: String, hexKey: String? = null): MessageEntity {
    val (title, body) = E2ee.decryptTitleBody(title, body, encrypted, hexKey)
    return MessageEntity(
        id = id,
        sub = sub,
        sendId = sendId,
        title = title,
        body = body,
        priority = priority,
        attachmentUri = attachmentUri,
        ackState = if (receiptId != null) AckState.PENDING else AckState.NONE,
        receiptId = receiptId,
    )
}

/** Boolean read tolerant of bool OR int (1) serialization of the flag. */
private fun JSONObject.booleanish(key: String, default: Boolean): Boolean =
    when (val v = opt(key)) {
        is Boolean -> v
        is Int -> v == 1
        is Long -> v == 1L
        else -> default
    }
