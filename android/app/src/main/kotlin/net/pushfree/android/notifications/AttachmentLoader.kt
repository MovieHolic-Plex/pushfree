package net.pushfree.android.notifications

import android.content.Context
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.net.Uri
import android.util.Log
import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.URL

/**
 * Loads an attachment bitmap for [android.app.Notification.BigPictureStyle].
 *
 * Implementations must never throw: return null on any failure so the caller
 * falls back to a text-only notification instead of dropping the message.
 */
fun interface AttachmentLoader {
    /** @return decoded bitmap, or null on any failure. Performs blocking IO. */
    fun load(uri: String): Bitmap?
}

/**
 * Downloads (http/https) or opens (content/file) an attachment and decodes it
 * into a [Bitmap]. Performs **blocking** IO: callers (FCM/UnifiedPush handlers,
 * the WorkManager outbox in todo 33) must invoke this off the main thread. Any
 * error — network, HTTP status, decode, oversize — yields null.
 */
class DefaultAttachmentLoader(
    private val context: Context,
    private val connectTimeoutMs: Int = 10_000,
    private val readTimeoutMs: Int = 10_000,
    private val maxBytes: Int = 4 * 1024 * 1024,
) : AttachmentLoader {
    override fun load(uri: String): Bitmap? = try {
        val parsed = Uri.parse(uri)
        when (parsed.scheme?.lowercase()) {
            "content", "file" -> context.contentResolver.openInputStream(parsed)?.use { decode(it) }
            else -> downloadHttp(uri)
        }
    } catch (t: Throwable) {
        Log.w(TAG, "attachment load failed: $uri", t)
        null
    }

    private fun downloadHttp(url: String): Bitmap? {
        return try {
            val conn = (URL(url).openConnection() as HttpURLConnection).apply {
                connectTimeout = connectTimeoutMs
                readTimeout = readTimeoutMs
                requestMethod = "GET"
                instanceFollowRedirects = true
            }
            try {
                if (conn.responseCode !in 200..299) return null
                val bytes = readBounded(conn.inputStream, maxBytes) ?: return null
                decode(ByteArrayInputStream(bytes))
            } finally {
                conn.disconnect()
            }
        } catch (t: Throwable) {
            Log.w(TAG, "attachment http download failed: $url", t)
            null
        }
    }

    private fun decode(input: InputStream): Bitmap? = BitmapFactory.decodeStream(input)

    private fun readBounded(input: InputStream, limit: Int): ByteArray? {
        val out = ByteArrayOutputStream()
        val buf = ByteArray(8 * 1024)
        var total = 0
        while (true) {
            val n = input.read(buf)
            if (n <= 0) break
            total += n
            if (total > limit) return null
            out.write(buf, 0, n)
        }
        return out.toByteArray()
    }

    private companion object {
        const val TAG = "PfAttachLoader"
    }
}
