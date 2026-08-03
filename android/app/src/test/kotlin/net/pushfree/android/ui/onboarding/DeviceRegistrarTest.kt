package net.pushfree.android.ui.onboarding

import kotlinx.coroutines.test.runTest
import net.pushfree.android.data.AckState
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.data.SubscriptionEntity
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

/**
 * Drives [OkHttpDeviceRegistrar] against a scripted [MockWebServer]: the full
 * login -> me -> app -> device flow, plus the URL-validation and auth-failure
 * branches that the add-server screen surfaces as snackbars.
 */
class DeviceRegistrarTest {

    private lateinit var server: MockWebServer
    private lateinit var registrar: OkHttpDeviceRegistrar

    @Before
    fun setUp() {
        server = MockWebServer()
        server.start()
        // Plain client (no shared state between tests); cookie jar is per-registrar.
        registrar = OkHttpDeviceRegistrar(
            client = OkHttpClient(),
            os = "test-os",
            model = "test-model",
        )
    }

    @After
    fun tearDown() {
        server.shutdown()
    }

    private fun ok(body: String) = MockResponse()
        .setResponseCode(200)
        .setHeader("Set-Cookie", "session=abc; Path=/")
        .setBody(body)

    @Test
    fun `normalize base accepts bare host and adds https`() {
        assertEquals("https://example.com", OkHttpDeviceRegistrar.normalizeBase("example.com"))
        assertEquals("https://example.com", OkHttpDeviceRegistrar.normalizeBase("https://example.com/"))
        assertEquals("http://localhost:8080", OkHttpDeviceRegistrar.normalizeBase("http://localhost:8080"))
    }

    @Test
    fun `normalize base rejects garbage, non-http schemes and paths`() {
        assertNull(OkHttpDeviceRegistrar.normalizeBase("not a url !!"))
        assertNull(OkHttpDeviceRegistrar.normalizeBase("ftp://example.com"))
        assertNull(OkHttpDeviceRegistrar.normalizeBase("https://example.com/path"))
        assertNull(OkHttpDeviceRegistrar.normalizeBase(""))
    }

    @Test
    fun `register completes full login to device flow`() = runTest {
        server.enqueue(ok("""{"status":1}"""))                                   // login
        server.enqueue(ok("""{"status":1,"user_key":"USERKEY123"}"""))           // me
        server.enqueue(MockResponse().setResponseCode(201).setBody(             // apps
            """{"status":1,"token":"APPTOKEN123"}""",
        ))
        server.enqueue(ok(                                                      // devices/login
            """{"status":1,"device_id":"DEV123","secret":"SECRET123"}""",
        ))

        val result = registrar.register(
            RegistrationInput(
                serverUrl = server.url("/").toString(),
                email = "user@example.com",
                password = "secret",
                deviceName = "phone",
            ),
        )

        assertTrue(result is RegistrationResult.Success)
        val sub = (result as RegistrationResult.Success).subscription
        assertEquals(server.url("/").toString().trimEnd('/'), sub.serverUrl)
        assertEquals("USERKEY123", sub.userKey)
        assertEquals("APPTOKEN123", sub.token)
        assertEquals("DEV123", sub.deviceId)
        assertEquals("SECRET123", sub.secret)

        // Exactly four requests in the right order.
        assertEquals(4, server.requestCount)
        assertEquals("/v1/accounts/login", server.takeRequest().path)
        assertEquals("/v1/accounts/me", server.takeRequest().path)
        assertEquals("/v1/apps", server.takeRequest().path)
        assertEquals("/1/devices/login.json", server.takeRequest().path)
    }

    @Test
    fun `invalid url yields failure without contacting server`() = runTest {
        val result = registrar.register(
            RegistrationInput("not a url !!", "a@b.com", "pw", "phone"),
        )
        assertTrue(result is RegistrationResult.Failure)
        assertEquals("Invalid server URL", (result as RegistrationResult.Failure).reason)
        assertEquals(0, server.requestCount)
    }

    @Test
    fun `wrong password surfaces a user-facing auth failure`() = runTest {
        server.enqueue(
            MockResponse().setResponseCode(401).setBody("""{"status":0,"errors":["bad"]}"""),
        )
        val result = registrar.register(
            RegistrationInput(server.url("/").toString(), "a@b.com", "wrong", "phone"),
        )
        assertTrue(result is RegistrationResult.Failure)
        assertEquals("Invalid email or password", (result as RegistrationResult.Failure).reason)
        // Stopped after the failed login; no me/apps/device calls.
        assertEquals(1, server.requestCount)
    }

    @Test
    fun `server error during device login surfaces device failure`() = runTest {
        server.enqueue(ok("""{"status":1}"""))
        server.enqueue(ok("""{"status":1,"user_key":"U"}"""))
        server.enqueue(MockResponse().setResponseCode(201).setBody("""{"status":1,"token":"T"}"""))
        server.enqueue(MockResponse().setResponseCode(500).setBody("boom"))
        val result = registrar.register(
            RegistrationInput(server.url("/").toString(), "a@b.com", "pw", "phone"),
        )
        assertTrue(result is RegistrationResult.Failure)
        assertEquals("Device registration failed", (result as RegistrationResult.Failure).reason)
    }

    @Suppress("unused")
    private fun sampleMessage(id: Long, priority: Int, receipt: String? = null): MessageEntity =
        MessageEntity(
            id = id,
            sub = "https://srv",
            sendId = id,
            title = "t",
            body = "b",
            priority = priority,
            attachmentUri = null,
            ackState = if (receipt != null) AckState.PENDING else AckState.NONE,
            receiptId = receipt,
        )

    @Suppress("unused")
    private fun sampleSub(url: String): SubscriptionEntity = SubscriptionEntity(
        serverUrl = url,
        userKey = "USERKEYUSERKEYUSERKEYUS",
        token = "APPTOKENAPPTOKENAPPTOKENAPP",
        deviceId = "DEVDEVDEVDEVDEVDEVDEVDEVDEV",
        secret = "SECRETSECRETSECRETSECRETSE",
    )
}
