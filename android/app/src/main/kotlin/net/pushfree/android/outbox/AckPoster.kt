package net.pushfree.android.outbox

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import java.net.URLEncoder
import java.nio.charset.StandardCharsets

/**
 * Outcome of a single ack POST attempt. The [AckWorker] maps each variant to a
 * [androidx.work.ListenableWorker.Result]:
 *  - [Success]            -> Result.success() (mark ACKED, dismiss notification)
 *  - [PermanentFailure]   -> Result.failure() (stop retrying; e.g. HTTP 404)
 *  - [TransientFailure]   -> Result.retry()   (re-queue with backoff)
 */
sealed interface AckPostResult {
    /** HTTP 200 with envelope `{"status":1,...}`. */
    data object Success : AckPostResult

    /** Permanent client error (HTTP 404): the receipt is gone / GC'd. Stop retrying. */
    data class PermanentFailure(val httpCode: Int) : AckPostResult

    /** Any other non-2xx response or an IO failure. Retryable. */
    data class TransientFailure(val httpCode: Int?, val message: String) : AckPostResult
}

/**
 * Sends `POST {serverUrl}/1/receipts/{receiptId}/acknowledge.json` with the
 * device [secret] as form data. Injectable so tests can drive each outcome
 * without opening real sockets.
 */
fun interface AckPoster {
    suspend fun post(serverUrl: String, receiptId: String, deviceId: String, secret: String): AckPostResult
}

/**
 * Production [AckPoster] built on [HttpURLConnection]. Follows the deliverable's
 * status classification: 200 + status==1 -> success; 404 -> permanent; anything
 * else (incl. IO errors) -> transient/retryable.
 */
object HttpUrlConnectionAckPoster : AckPoster {
    override suspend fun post(
        serverUrl: String,
        receiptId: String,
        deviceId: String,
        secret: String,
    ): AckPostResult = withContext(Dispatchers.IO) {
        val url = "${serverUrl.trimEnd('/')}/1/receipts/$receiptId/acknowledge.json"
        val form = "device_id=" + URLEncoder.encode(deviceId, "UTF-8") +
            "&secret=" + URLEncoder.encode(secret, "UTF-8")
        var conn: HttpURLConnection? = null
        try {
            conn = (URL(url).openConnection() as HttpURLConnection).apply {
                requestMethod = "POST"
                connectTimeout = CONNECT_TIMEOUT_MS
                readTimeout = READ_TIMEOUT_MS
                doOutput = true
                setRequestProperty("Content-Type", "application/x-www-form-urlencoded")
            }
            conn.outputStream.use { it.write(form.toByteArray(StandardCharsets.UTF_8)) }
            val code = conn.responseCode
            val body = runCatching {
                (conn.errorStream ?: conn.inputStream)?.bufferedReader()?.use { it.readText() }.orEmpty()
            }.getOrDefault("")
            when {
                code == 200 && statusIsOne(body) -> AckPostResult.Success
                code == 404 -> AckPostResult.PermanentFailure(404)
                code in 200..299 -> AckPostResult.TransientFailure(code, "unexpected envelope: $body")
                else -> AckPostResult.TransientFailure(code, body)
            }
        } catch (e: IOException) {
            AckPostResult.TransientFailure(null, e.message ?: "io error")
        } finally {
            conn?.disconnect()
        }
    }

    private fun statusIsOne(body: String): Boolean =
        runCatching { JSONObject(body).optInt("status", 0) == 1 }.getOrDefault(false)

    private const val CONNECT_TIMEOUT_MS = 15_000
    private const val READ_TIMEOUT_MS = 15_000
}
