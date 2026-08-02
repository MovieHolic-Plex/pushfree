package net.pushfree.android.ws

import okhttp3.OkHttpClient
import java.util.concurrent.TimeUnit

/**
 * Builds the OkHttp client for the WS transport.
 *
 * - readTimeout = 77s: the server emits a keepalive every 45s, so a healthy
 *   connection always produces a read within 45s; 77s = 45s keepalive + a
 *   generous margin (W2-ws transport constants) before the client gives up.
 * - pingInterval = 30s: OkHttp sends WebSocket ping frames every 30s so NAT /
 *   proxy idle timers cannot silently drop the TCP connection under the 77s
 *   read timeout.
 *
 * Both values are exposed as constants so tests assert the exact contract.
 */
object WsClientFactory {
    const val READ_TIMEOUT_MS = 77_000L
    const val PING_INTERVAL_MS = 30_000L

    fun build(): OkHttpClient = OkHttpClient.Builder()
        .readTimeout(READ_TIMEOUT_MS, TimeUnit.MILLISECONDS)
        .pingInterval(PING_INTERVAL_MS, TimeUnit.MILLISECONDS)
        .build()
}
