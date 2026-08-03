package net.pushfree.android.up

/**
 * UnifiedPush connector intent contract.
 *
 * The connector role (this app) receives pushes from an on-device distributor
 * app (ntfy, an FCM-replacement distributor, ...) via Android broadcast
 * intents. This object holds the public action strings and extra-key names the
 * distributor emits (connector inbound) and the actions the connector broadcasts
 * to ask the distributor to register/unregister (connector outbound).
 *
 * Spec: unifiedpush.org/developers/connector/android/. The action strings are
 * the published, stable connector constants. They are pinned as explicit
 * in-tree literals (rather than depending on the
 * `org.unifiedpush.android:connector` library artifact) so the build stays
 * hermetic and the APK stays small; the connector intent contract is a public
 * stable surface, not an internal API.
 */
object UnifiedPushContract {
    // --- Distributor -> Connector (handled by [PushfreeUpReceiver]) ---

    /** Distributor granted a new push endpoint URL (string extra [EXTRA_ENDPOINT]). */
    const val ACTION_NEW_ENDPOINT = "org.unifiedpush.android.connector.NEW_ENDPOINT"

    /** A push message arrived (byte-array extra [EXTRA_BYTES] = raw payload). */
    const val ACTION_MESSAGE_RECEIVED = "org.unifiedpush.android.connector.MESSAGE_RECEIVED"

    /** The distributor revoked / lost the registration (pushes stop). */
    const val ACTION_UNREGISTERED = "org.unifiedpush.android.connector.UNREGISTERED"

    /** The distributor acknowledged a registration request (informational). */
    const val ACTION_REGISTERED = "org.unifiedpush.android.connector.REGISTERED"

    // --- Connector -> Distributor (broadcast by [UnifiedPushDistributor]) ---

    /** Ask any installed distributor to register this instance. */
    const val ACTION_REGISTER = "org.unifiedpush.android.connector.REGISTER"

    /** Ask the distributor to unregister this instance. */
    const val ACTION_UNREGISTER = "org.unifiedpush.android.connector.UNREGISTER"

    // --- Intent extras ---

    /** Raw push payload bytes for [ACTION_MESSAGE_RECEIVED]. */
    const val EXTRA_BYTES = "bytes"

    /** Endpoint URL string for [ACTION_NEW_ENDPOINT]. */
    const val EXTRA_ENDPOINT = "endpoint"

    /** Application id (package) the distributor routes the instance to. */
    const val EXTRA_APPLICATION = "application"

    /** Per-application instance identifier (default instance is the empty string). */
    const val EXTRA_INSTANCE = "instance"

    /** Distributor-assigned token identifying this registration. */
    const val EXTRA_TOKEN = "token"

    /** Default instance name when the caller does not multiplex instances. */
    const val DEFAULT_INSTANCE = ""
}
