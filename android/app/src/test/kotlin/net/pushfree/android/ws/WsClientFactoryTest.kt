package net.pushfree.android.ws

import okhttp3.OkHttpClient
import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * Asserts the OkHttp client contract for the WS transport: a 77s read timeout
 * (server keepalive 45s + margin) and a 30s ping interval. Pure JVM — OkHttp
 * exposes both as read-only public properties.
 *
 * Acceptance: readTimeout == 77s and pingInterval == 30s asserted on client config.
 */
class WsClientFactoryTest {

    @Test
    fun read_timeout_is_77s_and_ping_interval_is_30s() {
        val client: OkHttpClient = WsClientFactory.build()
        try {
            assertEquals(77_000L, client.readTimeoutMillis.toLong())
            assertEquals(30_000L, client.pingIntervalMillis.toLong())
        } finally {
            client.dispatcher.executorService.shutdown()
            client.connectionPool.evictAll()
        }
    }
}
