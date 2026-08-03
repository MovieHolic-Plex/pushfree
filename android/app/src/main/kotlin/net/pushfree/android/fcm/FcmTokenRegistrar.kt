package net.pushfree.android.fcm

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
 * Registers (and re-registers on rotation) the FCM registration token with the
 * pushfree server so the server-side FCM v1 channel (todo 16) can route to this
 * device. The server stores it as `devices.fcm_token` (the column the server
 * reads and clears via `ClearFCMToken`); this is the client half of that
 * contract.
 *
 * Endpoint: `POST {serverUrl}/1/devices/fcm_token.json` authenticated with the
 * device's `device_id` + `secret` (the Open Client credentials issued by
 * `/1/devices/login.json`, todo 13). Mirrors the server's device-login path
 * shape.
 *
 * Not unit-tested (no network in the suite, per spec); failures are logged and
 * the next token-rotation event retries. This class is intentionally thin — it
 * owns only the HTTP call, not persistence or backoff (a WorkManager-backed
 * retry queue mirrors the ack outbox, todo 33, when the server endpoint lands).
 *
 * @param client injectable for determinism; defaults to a short-timeout shared
 *        client so a wedged server cannot stall the service indefinitely.
 */
class FcmTokenRegistrar(
    private val context: Context,
    private val client: OkHttpClient = defaultClient(),
) {
    /**
     * POST the current [token] to the configured server. Returns true on a 2xx
     * response, false otherwise (including no subscription configured). Never
     * throws — callers run this in a fire-and-log coroutine.
     */
    suspend fun register(token: String): Boolean = withContext(Dispatchers.IO) {
        val sub = AckOutboxServices.database(context).subscriptionDao().getAll().firstOrNull()
        if (sub == null) {
            Log.w(TAG, "token registration skipped: no subscription configured")
            return@withContext false
        }
        val url = sub.serverUrl.trimEnd('/') + PATH
        val body = JSONObject()
            .put("device_id", sub.deviceId)
            .put("secret", sub.secret)
            .put("fcm_token", token)
            .toString()
        val request = Request.Builder()
            .url(url)
            .post(body.toRequestBody(JSON))
            .build()
        val ok = withTimeoutOrNull(TIMEOUT_MS) {
            runCatching { client.newCall(request).execute() }
                .getOrNull()?.use { resp -> resp.code in 200..299 }
        } ?: false
        if (!ok) Log.w(TAG, "token registration to $url failed")
        ok
    }

    private companion object {
        const val TAG = "FcmTokenRegistrar"
        const val PATH = "/1/devices/fcm_token.json"
        const val TIMEOUT_MS = 10_000L
        val JSON = "application/json; charset=utf-8".toMediaType()

        // Short timeouts: the service calls this on token rotation; a wedged
        // server must not pin the FCM service worker indefinitely.
        fun defaultClient(): OkHttpClient = OkHttpClient.Builder()
            .connectTimeout(5, TimeUnit.SECONDS)
            .readTimeout(5, TimeUnit.SECONDS)
            .build()
    }
}
