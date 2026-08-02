package net.pushfree.android.ws

import org.json.JSONObject

/**
 * Server-protocol framing constants for the `/1/ws` client transport
 * (committed in `server/internal/hub`, todo 13).
 */
object WsProtocol {
    const val PATH = "/1/ws"
    const val QUERY_SINCE = "since"

    const val TYPE_LOGIN = "login"
    const val TYPE_OPEN = "open"
    const val TYPE_MESSAGE = "message"
    const val TYPE_KEEPALIVE = "keepalive"

    const val FIELD_TYPE = "type"
    const val FIELD_DEVICE_ID = "device_id"
    const val FIELD_SECRET = "secret"
    const val FIELD_LAST_MESSAGE_ID = "last_message_id"
    const val FIELD_ID = "id"
    const val FIELD_SEND_ID = "send_id"
    const val FIELD_TITLE = "title"
    const val FIELD_BODY = "body"
    const val FIELD_PRIORITY = "priority"
    const val FIELD_RECEIPT = "receipt"
    const val FIELD_RECEIPT_ID = "receipt_id"
    const val FIELD_ATTACHMENT = "attachment"
}

/**
 * A parsed server `{"type":"message",...}` frame. Maps onto a Room
 * [net.pushfree.android.data.MessageEntity] by the foreground service.
 */
data class WsMessage(
    val id: Long,
    val sendId: Long,
    val title: String?,
    val body: String,
    val priority: Int,
    val receiptId: String?,
    val attachmentUri: String?,
)

/**
 * Streamed transport events surfaced by [WsTransport.events].
 */
sealed interface WsEvent {
    /** WS upgrade succeeded and the login line was sent. */
    data object Connected : WsEvent

    /** Server `open` frame received; [lastMessageId] is the server high-water mark. */
    data class Open(val lastMessageId: Long) : WsEvent

    /** A decoded message frame. The cursor has been advanced to [message]'s id. */
    data class Message(val message: WsMessage) : WsEvent

    /** Server keepalive frame (no-op; resets the read-timeout window). */
    data object Keepalive : WsEvent

    /** A connection attempt failed; the transport will back off and retry. */
    data class Error(val reason: String) : WsEvent

    /** The connection ended; the transport reconnects after the Full-Jitter delay for [attempt]. */
    data class Reconnecting(val attempt: Int) : WsEvent
}

/** Builds the first-line login JSON frame the client sends on WS open. */
internal fun buildLoginLine(deviceId: String, secret: String): String =
    JSONObject()
        .put(WsProtocol.FIELD_TYPE, WsProtocol.TYPE_LOGIN)
        .put(WsProtocol.FIELD_DEVICE_ID, deviceId)
        .put(WsProtocol.FIELD_SECRET, secret)
        .toString()

/** Parses a server text frame into a typed [WsEvent], or null if unrecognized. */
internal fun parseFrame(raw: String): WsEvent? {
    val json = JSONObject(raw)
    return when (json.optString(WsProtocol.FIELD_TYPE)) {
        WsProtocol.TYPE_OPEN -> WsEvent.Open(json.optLong(WsProtocol.FIELD_LAST_MESSAGE_ID, 0L))
        WsProtocol.TYPE_MESSAGE -> WsEvent.Message(parseMessage(json))
        WsProtocol.TYPE_KEEPALIVE -> WsEvent.Keepalive
        else -> null
    }
}

private fun parseMessage(json: JSONObject): WsMessage {
    val id = json.optLong(WsProtocol.FIELD_ID, 0L)
    return WsMessage(
        id = id,
        sendId = json.optLong(WsProtocol.FIELD_SEND_ID, id),
        title = json.stringOrNull(WsProtocol.FIELD_TITLE),
        body = json.optString(WsProtocol.FIELD_BODY),
        priority = json.optInt(WsProtocol.FIELD_PRIORITY, 0),
        receiptId = json.stringOrNull(WsProtocol.FIELD_RECEIPT_ID)
            ?: json.stringOrNull(WsProtocol.FIELD_RECEIPT),
        attachmentUri = json.stringOrNull(WsProtocol.FIELD_ATTACHMENT),
    )
}

/** Null-safe string read: absent key -> null; present -> its string value. */
private fun JSONObject.stringOrNull(key: String): String? =
    if (has(key)) optString(key).takeUnless { it.isEmpty() } else null
