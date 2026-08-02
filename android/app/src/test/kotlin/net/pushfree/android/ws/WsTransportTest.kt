package net.pushfree.android.ws

import androidx.test.ext.junit.runners.AndroidJUnit4
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Job
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import okhttp3.OkHttpClient
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config
import java.util.concurrent.TimeUnit

/**
 * MockWebServer-driven tests for [WsTransport]. Robolectric is used only so
 * `org.json` resolves on the JVM; the WS + MockWebServer plumbing is pure Java.
 *
 * Covers the acceptance:
 *  - login line sent as the first frame;
 *  - disconnect -> backoff reconnect that resumes at exactly the since-cursor
 *    (only strictly-newer messages replay);
 *  - server HTTP 500 -> backoff reconnect that survives and still delivers.
 *
 * No real network, no sleeps for timing: reconnect latency is virtualised via a
 * no-op sleeper, and every wait is event-driven (server-side frame capture or
 * `takeRequest`), so the suite is deterministic.
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34])
class WsTransportTest {

    private lateinit var server: MockWebServer
    private lateinit var client: OkHttpClient
    private lateinit var cursor: InMemoryCursor

    @Before
    fun setUp() {
        server = MockWebServer()
        server.start()
        // Short read timeout keeps a stuck test failing fast instead of hanging.
        client = OkHttpClient.Builder()
            .readTimeout(READ_TIMEOUT_S, TimeUnit.SECONDS)
            .build()
        cursor = InMemoryCursor()
    }

    @After
    fun tearDown() {
        client.dispatcher.executorService.shutdown()
        client.connectionPool.evictAll()
        server.shutdown()
    }

    // 1. The login line is sent as the very first client text frame.
    @Test
    fun sends_login_line_as_first_frame() = runBlocking {
        val firstClientFrame = CompletableDeferred<String>()
        server.enqueue(
            MockResponse().withWebSocketUpgrade(
                object : WebSocketListener() {
                    override fun onOpen(ws: WebSocket, response: Response) {
                        ws.send("""{"type":"open","last_message_id":0}""")
                    }

                    override fun onMessage(ws: WebSocket, text: String) {
                        firstClientFrame.complete(text)
                        ws.close(NORMAL_CLOSURE, "ok")
                    }
                },
            ),
        )
        val transport = WsTransport(
            client,
            config(deviceId = DEVICE_ID, secret = SECRET),
            InMemoryCursor(),
            sleeper = NOOP_SLEEPER,
        )

        val job = launch { transport.events().collect { } }
        val raw = withTimeout(AWAIT_MS) { firstClientFrame.await() }
        job.cancelAndJoin()

        val login = JSONObject(raw)
        assertEquals("login", login.getString("type"))
        assertEquals(DEVICE_ID, login.getString("device_id"))
        assertEquals(SECRET, login.getString("secret"))
    }

    // 2. Disconnect -> reconnect resumes at exactly the since-cursor: only
    //    strictly-newer messages replay, and the reconnect URL carries since=<cursor>.
    @Test
    fun reconnect_replays_only_messages_after_since_cursor() = runBlocking {
        // Upgrade 1: open(hwm=100) + messages 101, 102, then close -> reconnect.
        server.enqueue(
            wsUpgrade { ws ->
                ws.send("""{"type":"open","last_message_id":100}""")
                ws.send(message(101, 1001, title = "a", body = "A"))
                ws.send(message(102, 1002, title = "b", body = "B"))
                ws.close(NORMAL_CLOSURE, "done")
            },
        )
        // Upgrade 2: server replays only id>102 (here just 103).
        server.enqueue(
            wsUpgrade { ws ->
                ws.send("""{"type":"open","last_message_id":102}""")
                ws.send(message(103, 1003, title = "c", body = "C"))
            },
        )

        val transport = WsTransport(client, config(), cursor, sleeper = NOOP_SLEEPER)

        val seen = mutableListOf<Long>()
        val got103 = CompletableDeferred<Unit>()
        val job = launch {
            transport.events().collect { ev ->
                (ev as? WsEvent.Message)?.let {
                    seen += it.message.id
                    if (it.message.id == 103L) got103.complete(Unit)
                }
            }
        }
        withTimeout(AWAIT_MS) { got103.await() }
        job.cancelAndJoin()

        // Exactly 101, 102, 103 emitted — no duplicate replay of 101/102.
        assertEquals(listOf(101L, 102L, 103L), seen)
        // Final cursor reflects the last persisted id (103); the reconnect used
        // since=102 (proven by req2.path below) — i.e. only the unseen tail replayed.
        assertEquals(103L, cursor.read())

        val req1 = server.takeRequest()
        val req2 = server.takeRequest()
        assertTrue("fresh connect must start at since=0: ${req1.path}", req1.path!!.contains("since=0"))
        assertTrue("reconnect must resume since=102: ${req2.path}", req2.path!!.contains("since=102"))
    }

    // 3. An existing cursor is honoured on the very first connect (no history replay).
    @Test
    fun first_connect_resumes_from_persisted_cursor() = runBlocking {
        cursor.seed(55L)
        server.enqueue(
            wsUpgrade { ws ->
                ws.send("""{"type":"open","last_message_id":55}""")
                ws.send(message(56, 2001, title = "x", body = "X"))
            },
        )
        val transport = WsTransport(client, config(), cursor, sleeper = NOOP_SLEEPER)
        val got56 = CompletableDeferred<Unit>()
        val job = launch {
            transport.events().collect { ev ->
                if (ev is WsEvent.Message && ev.message.id == 56L) got56.complete(Unit)
            }
        }
        withTimeout(AWAIT_MS) { got56.await() }
        job.cancelAndJoin()

        val req = server.takeRequest()
        assertTrue("must resume since=55: ${req.path}", req.path!!.contains("since=55"))
    }

    // 4. Server returns HTTP 500 on the WS upgrade -> backoff reconnect without crash.
    @Test
    fun server_500_triggers_backoff_reconnect_without_crash() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(500))
        server.enqueue(
            wsUpgrade { ws ->
                ws.send("""{"type":"open","last_message_id":0}""")
                ws.send(message(1, 3001, title = "r", body = "recovered"))
            },
        )
        val transport = WsTransport(client, config(), InMemoryCursor(), sleeper = NOOP_SLEEPER)

        val firstError = CompletableDeferred<Unit>()
        val firstMessage = CompletableDeferred<Unit>()
        val job = launch {
            transport.events().collect { ev ->
                when (ev) {
                    is WsEvent.Error -> firstError.complete(Unit)
                    is WsEvent.Message -> firstMessage.complete(Unit)
                    else -> Unit
                }
            }
        }
        withTimeout(AWAIT_MS) { firstError.await() }
        withTimeout(AWAIT_MS) { firstMessage.await() }
        job.cancelAndJoin()
    }

    // 5. Keepalive frames are decoded (no crash) and surfaced.
    @Test
    fun keepalive_frame_is_decoded() = runBlocking {
        val gotKeepalive = CompletableDeferred<Unit>()
        server.enqueue(
            wsUpgrade { ws ->
                ws.send("""{"type":"open","last_message_id":0}""")
                ws.send("""{"type":"keepalive"}""")
            },
        )
        val transport = WsTransport(client, config(), InMemoryCursor(), sleeper = NOOP_SLEEPER)
        val job = launch {
            transport.events().collect { ev ->
                if (ev is WsEvent.Keepalive) gotKeepalive.complete(Unit)
            }
        }
        withTimeout(AWAIT_MS) { gotKeepalive.await() }
        job.cancelAndJoin()
    }

    // ---- helpers ----

    private fun config(deviceId: String = DEVICE_ID, secret: String = SECRET): WsConfig =
        WsConfig(serverUrl = server.url("/").toString(), deviceId = deviceId, secret = secret)

    private fun wsUpgrade(script: (WebSocket) -> Unit): MockResponse =
        MockResponse().withWebSocketUpgrade(
            object : WebSocketListener() {
                override fun onOpen(ws: WebSocket, response: Response) = script(ws)
            },
        )

    private fun message(id: Long, sendId: Long, title: String, body: String): String =
        """{"type":"message","id":$id,"send_id":$sendId,"title":"$title","body":"$body","priority":0}"""

    private class InMemoryCursor : WsCursorStore {
        @Volatile private var id: Long? = null

        fun seed(value: Long) {
            id = value
        }

        override suspend fun read(): Long? = id

        override suspend fun write(value: Long) {
            val current = id
            if (current == null || value > current) id = value
        }
    }

    private companion object {
        const val DEVICE_ID = "dev-xyz"
        const val SECRET = "s33kr3t"
        const val NORMAL_CLOSURE = 1000
        const val READ_TIMEOUT_S = 5L
        const val AWAIT_MS = 15_000L

        // Virtualise reconnect latency: the backoff *function* is tested for
        // timing in WsBackoffTest; here the reconnect must be immediate.
        val NOOP_SLEEPER: suspend (kotlin.time.Duration) -> Unit = {}
    }
}
