package net.pushfree.android.up

/**
 * A typed UnifiedPush lifecycle / message event decoded from a distributor
 * intent. The pure decoder [decodeUpIntent] is unit-tested without Android;
 * [UpDispatcher] turns each variant into the appropriate side effect (Room
 * insert for a message, register/unregister for the lifecycle events).
 *
 * This is the "endpoint handling + lifecycle mapping" required by the spec:
 * each distributor intent action maps to exactly one variant, and a malformed
 * message payload maps to [MessageIgnored] rather than crashing.
 */
sealed interface UpIntent {
    /**
     * Distributor granted a new push endpoint URL. The connector records it and
     * (best-effort) tells the server to target this device via the endpoint.
     */
    data class NewEndpoint(val endpoint: String) : UpIntent

    /** A push arrived and parsed into an [UpMessage]; persist + notify. */
    data class MessageReceived(val message: UpMessage) : UpIntent

    /** A [UnifiedPushContract.ACTION_MESSAGE_RECEIVED] intent whose payload did not parse. */
    object MessageIgnored : UpIntent

    /** Distributor revoked the registration -> tell the server to unregister this device. */
    object Unregistered : UpIntent

    /** Distributor acknowledged a registration request (informational only). */
    object Registered : UpIntent

    /** Unrecognized action, or a NEW_ENDPOINT missing its endpoint extra. */
    object Unknown : UpIntent
}

/**
 * Decode a distributor intent into a typed [UpIntent]. Pure: no Android
 * framework types are touched (the caller extracts the action string, the
 * endpoint extra, and the bytes extra from the intent), so this runs as a plain
 * JVM unit test.
 *
 * - NEW_ENDPOINT without an endpoint string -> [UpIntent.Unknown] (malformed).
 * - MESSAGE_RECEIVED with no/payload bytes that fail to parse -> [UpIntent.MessageIgnored].
 * - Any other action -> [UpIntent.Unknown].
 */
fun decodeUpIntent(action: String?, endpoint: String?, messageBytes: ByteArray?): UpIntent =
    when (action) {
        UnifiedPushContract.ACTION_NEW_ENDPOINT -> {
            val ep = endpoint?.takeIf { it.isNotBlank() }
            if (ep != null) UpIntent.NewEndpoint(ep) else UpIntent.Unknown
        }
        UnifiedPushContract.ACTION_MESSAGE_RECEIVED -> {
            val msg = messageBytes?.let { parseUpMessage(it) }
            if (msg != null) UpIntent.MessageReceived(msg) else UpIntent.MessageIgnored
        }
        UnifiedPushContract.ACTION_UNREGISTERED -> UpIntent.Unregistered
        UnifiedPushContract.ACTION_REGISTERED -> UpIntent.Registered
        else -> UpIntent.Unknown
    }
