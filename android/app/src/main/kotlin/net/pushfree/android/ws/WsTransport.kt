package net.pushfree.android.ws

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.channels.SendChannel
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.channelFlow
import kotlinx.coroutines.isActive
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import kotlin.random.Random
import kotlin.time.Duration

/** Connection target + credentials for a single subscription's WS transport. */
data class WsConfig(
    val serverUrl: String,
    val deviceId: String,
    val secret: String,
)

/**
 * Resume-cursor store backing since-replay. [read] returns the highest server
 * message id already observed (null = fresh subscription, no replay); [write]
 * advances it. Production uses [RoomWsCursorStore]; tests inject a fake.
 */
interface WsCursorStore {
    suspend fun read(): Long?
    suspend fun write(id: Long)
}

/**
 * Persistent WebSocket transport for the pushfree `/1/ws` protocol.
 *
 * - Builds `GET {serverUrl}/1/ws?since=<cursor>` and sends the login line
 *   `{"type":"login",...}` as the first frame on open.
 * - Streams decoded [WsEvent]s (Connected, Open, Message, Keepalive, ...).
 * - On any disconnect or failure, reconnects after a Full-Jitter backoff delay
 *   (base 1s, cap 60s — see [WsBackoff]); the attempt counter resets to 0 once a
 *   connection opens, so a long-lived connection never accrues a reconnect penalty.
 * - Advances the persisted since-cursor as each message frame arrives, so a
 *   reconnect replays only strictly-newer messages.
 *
 * [client], [cursor], [sleeper] and [random] make the reconnect loop
 * deterministic in tests (MockWebServer + no-op sleeper + seeded random).
 */
class WsTransport(
    private val client: OkHttpClient,
    private val config: WsConfig,
    private val cursor: WsCursorStore,
    private val sleeper: suspend (Duration) -> Unit = { delay(it.inWholeMilliseconds) },
    private val random: Random = Random.Default,
) {
    /**
     * Cold flow running the connect -> stream -> disconnect -> backoff loop.
     * Collecting it drives a single transport session; cancelling collection
     * tears down the live WebSocket.
     */
    fun events(): Flow<WsEvent> = channelFlow {
        var attempt = 0
        while (isActive) {
            val since = cursor.read() ?: 0L
            val opened = try {
                connectOnce(since, channel)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                // Non-fatal: surface and retry after backoff. Never crashes the app.
                channel.trySend(WsEvent.Error(e.message ?: "connect error"))
                false
            }
            // Reset backoff once a connection opened successfully; only accrue
            // attempts across back-to-back failures.
            attempt = if (opened) 0 else attempt + 1
            if (!isActive) break
            channel.trySend(WsEvent.Reconnecting(attempt))
            sleeper(WsBackoff.delay(attempt, random))
        }
    }

    /**
     * Opens one WS connection, streams frames until the server closes/fails,
     * and returns true iff the upgrade reached onOpen (login was sent).
     */
    private suspend fun connectOnce(since: Long, out: SendChannel<WsEvent>): Boolean {
        val frames = Channel<String>(Channel.UNLIMITED)
        val opened = java.util.concurrent.atomic.AtomicBoolean(false)
        val request = Request.Builder()
            .url(wsUrl(since))
            .build()
        coroutineScope {
            val ws = client.newWebSocket(request, object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) {
                    opened.set(true)
                    webSocket.send(buildLoginLine(config.deviceId, config.secret))
                    // trySend (non-suspending) since OkHttp calls us off-coroutine.
                    out.trySend(WsEvent.Connected)
                }

                override fun onMessage(webSocket: WebSocket, text: String) {
                    // Bridge the off-coroutine callback onto the coroutine loop
                    // via an unbounded channel; processed below with suspend IO.
                    frames.trySend(text)
                }

                override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                    webSocket.close(NORMAL_CLOSURE, null)
                }

                override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                    frames.close()
                }

                override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                    out.trySend(WsEvent.Error(t.message ?: "ws failure"))
                    frames.close()
                }
            })
            try {
                for (raw in frames) {
                    val parsed = parseFrame(raw)
                    when (parsed) {
                        is WsEvent.Message -> {
                            // Persist the resume cursor BEFORE surfacing, so a
                            // crash between emit and persist cannot lose the
                            // high-water mark; reconnect replays only the tail.
                            if (parsed.message.id > 0L) cursor.write(parsed.message.id)
                        }
                        is WsEvent.Open -> {
                            // Seed a fresh cursor from the server high-water mark
                            // so first connect does not replay history.
                            if (cursor.read() == null && parsed.lastMessageId > 0L) {
                                cursor.write(parsed.lastMessageId)
                            }
                        }
                        else -> Unit
                    }
                    if (parsed != null) out.send(parsed)
                }
            } finally {
                ws.cancel()
            }
        }
        return opened.get()
    }

    private fun wsUrl(since: Long): String =
        config.serverUrl.trimEnd('/') + WsProtocol.PATH + "?" + WsProtocol.QUERY_SINCE + "=" + since

    private companion object {
        const val NORMAL_CLOSURE = 1000
    }
}
