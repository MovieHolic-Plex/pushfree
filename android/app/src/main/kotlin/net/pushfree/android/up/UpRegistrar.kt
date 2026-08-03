package net.pushfree.android.up

import android.content.Context
import android.util.Log
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
import net.pushfree.android.outbox.AckOutboxServices
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
import java.util.concurrent.TimeUnit

/**
 * Owns the UnifiedPush device<->server registration lifecycle.
 *
 * The connector maps the distributor's lifecycle intents onto this interface:
 *  - [onNewEndpoint] on [UnifiedPushContract.ACTION_NEW_ENDPOINT]
 *  - [onUnregistered] on [UnifiedPushContract.ACTION_UNREGISTERED]
 *
 * "Server-call record": the registrar records the currently-registered endpoint
 * + a registered flag locally so the rest of the app (and the tests) can observe
 * whether the device is known to the server over UP. [onNewEndpoint] sets it;
 * [onUnregistered] clears it (the manual-QA failure scenario: "device removed
 * from server-call record"). The HTTP sync to the server is best-effort and
 * logged — it mirrors [net.pushfree.android.fcm.FcmTokenRegistrar] and is not
 * unit-tested (no network in the suite, per spec); the local record transitions
 * are what the tests assert.
 */
interface UpRegistrar {
    /** NEW_ENDPOINT: record the endpoint and (best-effort) sync to the server. */
    suspend fun onNewEndpoint(endpoint: String): Boolean

    /** UNREGISTERED: clear the record and (best-effort) tell the server. */
    suspend fun onUnregistered(): Boolean

    /** The endpoint currently registered with the server, or null if none. */
    fun currentEndpoint(): String?

    /** Whether the device is currently registered over UnifiedPush. */
    fun isRegistered(): Boolean
}

/**
 * In-memory, network-free [UpRegistrar] used by tests and as the fallback when
 * no server is configured. Records every call in [calls] (in order) so lifecycle
 * mapping is observable: the manual-QA failure scenario asserts
 * [currentEndpoint] is null after [onUnregistered].
 */
class RecordingUpRegistrar : UpRegistrar {
    @Volatile private var endpoint: String? = null

    /** Ordered call log, e.g. ["register:https://...", "unregister"]. */
    val calls = mutableListOf<String>()

    override suspend fun onNewEndpoint(endpoint: String): Boolean {
        synchronized(calls) { calls += "register:$endpoint" }
        this.endpoint = endpoint
        return true
    }

    override suspend fun onUnregistered(): Boolean {
        synchronized(calls) { calls += "unregister" }
        endpoint = null
        return true
    }

    override fun currentEndpoint(): String? = endpoint

    override fun isRegistered(): Boolean = endpoint != null
}

/**
 * OkHttp-backed [UpRegistrar]. Records state locally (the tested surface) and
 * performs a best-effort POST of the distributor-assigned endpoint to the
 * pushfree server so the server can route pushes to this device over UP.
 *
 * Endpoint shape mirrors the FCM token registrar (todo 30): a devices-family
 * route carrying the Open Client device_id + secret. The server-side counterpart
 * for UP is forward-compatible (the connector is delivered before that route
 * ships, exactly as the FCM transport was); a missing route fails gracefully —
 * logged, never thrown — and the local record still reflects the lifecycle so
 * the user-facing transport state stays correct. Failures retry on the next
 * NEW_ENDPOINT / app start (the distributor re-sends NEW_ENDPOINT on register).
 */
class OkHttpUpRegistrar(
    private val context: Context,
    private val client: OkHttpClient = defaultClient(),
) : UpRegistrar {

    @Volatile private var endpoint: String? = null

    override suspend fun onNewEndpoint(newEndpoint: String): Boolean {
        endpoint = newEndpoint
        return sync(PATH_REGISTER) { body -> body.put("endpoint", newEndpoint) }
    }

    override suspend fun onUnregistered(): Boolean {
        endpoint = null
        return sync(PATH_UNREGISTER) { it }
    }

    override fun currentEndpoint(): String? = endpoint

    override fun isRegistered(): Boolean = endpoint != null

    private suspend fun sync(path: String, decorate: (JSONObject) -> JSONObject): Boolean =
        withContext(Dispatchers.IO) {
            val sub = AckOutboxServices.database(context).subscriptionDao().getAll().firstOrNull()
            if (sub == null) {
                Log.w(TAG, "UP sync skipped ($path): no subscription configured")
                return@withContext false
            }
            val body = decorate(
                JSONObject()
                    .put("device_id", sub.deviceId)
                    .put("secret", sub.secret),
            ).toString()
            val request = Request.Builder()
                .url(sub.serverUrl.trimEnd('/') + path)
                .post(body.toRequestBody(JSON))
                .build()
            val ok = withTimeoutOrNull(TIMEOUT_MS) {
                runCatching { client.newCall(request).execute() }
                    .getOrNull()?.use { it.code in 200..299 }
            } ?: false
            if (!ok) Log.w(TAG, "UP sync failed: $path -> $ok")
            ok
        }

    private companion object {
        const val TAG = "OkHttpUpRegistrar"
        const val PATH_REGISTER = "/1/devices/up_endpoint.json"
        const val PATH_UNREGISTER = "/1/devices/up_unregister.json"
        const val TIMEOUT_MS = 10_000L
        val JSON = "application/json; charset=utf-8".toMediaType()

        fun defaultClient(): OkHttpClient = OkHttpClient.Builder()
            .connectTimeout(5, TimeUnit.SECONDS)
            .readTimeout(5, TimeUnit.SECONDS)
            .build()
    }
}
