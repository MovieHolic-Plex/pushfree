package net.pushfree.android.fcm

/**
 * A parsed pushfree FCM data-only message. Field keys mirror the `/1/ws` frame
 * (todo 13, `WsProtocol`) so a message looks identical regardless of transport:
 *
 * - `id`            server message id (required, must parse as a positive long)
 * - `send_id`       parent send id; defaults to `id` when absent
 * - `title`         optional title
 * - `body` / `message`   message text (required; the WS frame uses `message`)
 * - `priority`      Pushover priority -2..2; defaults to 0 when absent/unparseable
 * - `receipt_id` / `receipt`  receipt id for p2 messages (optional)
 * - `attachment`    attachment URI (optional)
 *
 * This is the contract the server-side FCM v1 channel (todo 16) populates when
 * it builds the `Outbound.Data` map; the client mirrors the field names here.
 */
data class FcmPayload(
    val id: Long,
    val sendId: Long,
    val title: String?,
    val body: String,
    val priority: Int,
    val receiptId: String?,
    val attachmentUri: String?,
)

/**
 * Parse an FCM data-only message payload (`Map<String, String>`) into an
 * [FcmPayload].
 *
 * Returns `null` for a corrupt/malformed payload — specifically when the
 * required `id` is missing/non-numeric/non-positive, or when neither `body`
 * nor `message` is present. Callers ([PushfreeFcmService]) treat `null` as
 * "ignore + log" so a bad payload never crashes the app or posts a bogus
 * notification (adversarial: a missing id is silently dropped, never counted
 * as a successful parse).
 *
 * This is a pure function: no Android or Firebase types are touched, so it runs
 * as a plain JVM unit test with no Robolectric/emulator/network.
 */
fun parseFcmPayload(data: Map<String, String>): FcmPayload? {
    val id = data["id"]?.toLongOrNull() ?: return null
    if (id <= 0L) return null
    val body = data["body"]?.takeIf { it.isNotEmpty() }
        ?: data["message"]?.takeIf { it.isNotEmpty() }
        ?: return null
    val sendId = data["send_id"]?.toLongOrNull() ?: id
    val title = data["title"]?.takeIf { it.isNotEmpty() }
    // A non-numeric priority falls back to the Pushover default (0); it is not
    // treated as corrupt because the message is still deliverable.
    val priority = data["priority"]?.toIntOrNull() ?: 0
    val receiptId = data["receipt_id"]?.takeIf { it.isNotEmpty() }
        ?: data["receipt"]?.takeIf { it.isNotEmpty() }
    val attachmentUri = data["attachment"]?.takeIf { it.isNotEmpty() }
    return FcmPayload(
        id = id,
        sendId = sendId,
        title = title,
        body = body,
        priority = priority,
        receiptId = receiptId,
        attachmentUri = attachmentUri,
    )
}
