package net.pushfree.android.ui.onboarding

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import net.pushfree.android.data.SubscriptionEntity
import okhttp3.Cookie
import okhttp3.CookieJar
import okhttp3.HttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.toHttpUrlOrNull
import org.json.JSONObject
import java.io.IOException
import java.util.concurrent.TimeUnit

/** Input collected by the add-server screen. */
data class RegistrationInput(
    val serverUrl: String,
    val email: String,
    val password: String,
    val deviceName: String,
)

/** Outcome of an onboarding attempt. [Failure.reason] is a user-facing snackbar string. */
sealed interface RegistrationResult {
    data class Success(val subscription: SubscriptionEntity) : RegistrationResult
    data class Failure(val reason: String) : RegistrationResult
}

/**
 * Registers this device against a pushfree server (todo 13/17 protocol):
 *  1. POST /v1/accounts/login    - email + password -> session cookie
 *  2. GET  /v1/accounts/me       - resolve user_key
 *  3. POST /v1/apps              - provision an app token
 *  4. POST /1/devices/login.json - issue device_id + secret
 */
fun interface DeviceRegistrar {
    suspend fun register(input: RegistrationInput): RegistrationResult
}

/** OkHttp-backed registrar with a per-instance in-memory cookie jar. */
class OkHttpDeviceRegistrar(
    private val client: OkHttpClient = defaultClient(),
    private val os: String = "android",
    private val model: String = "",
) : DeviceRegistrar {

    override suspend fun register(input: RegistrationInput): RegistrationResult =
        withContext(Dispatchers.IO) {
            val base = normalizeBase(input.serverUrl)
                ?: return@withContext RegistrationResult.Failure("Invalid server URL")

            if (input.email.isBlank() || input.password.isBlank()) {
                return@withContext RegistrationResult.Failure("Email and password are required")
            }
            val deviceName = input.deviceName.ifBlank { "android" }

            try {
                val login = jsonRequest(
                    "$base/v1/accounts/login",
                    JSONObject().put("email", input.email).put("password", input.password),
                )
                if (login.first != 200 || !statusIsOne(login.second)) {
                    return@withContext RegistrationResult.Failure("Invalid email or password")
                }

                val me = getRequest("$base/v1/accounts/me")
                if (me.first != 200) {
                    return@withContext RegistrationResult.Failure("Could not load account")
                }
                val userKey = JSONObject(me.second).optString("user_key").takeIf { it.isNotEmpty() }
                    ?: return@withContext RegistrationResult.Failure("Could not load account")

                val app = jsonRequest(
                    "$base/v1/apps",
                    JSONObject().put("name", "pushfree-$deviceName"),
                )
                if (app.first !in 200..299) {
                    return@withContext RegistrationResult.Failure("Could not create app token")
                }
                val token = JSONObject(app.second).optString("token").takeIf { it.isNotEmpty() }
                    ?: return@withContext RegistrationResult.Failure("Could not create app token")

                val dev = jsonRequest(
                    "$base/1/devices/login.json",
                    JSONObject().put("name", deviceName).put("os", os).put("model", model),
                )
                if (dev.first != 200 || !statusIsOne(dev.second)) {
                    return@withContext RegistrationResult.Failure("Device registration failed")
                }
                val devJson = JSONObject(dev.second)
                val deviceId = devJson.optString("device_id").takeIf { it.isNotEmpty() }
                    ?: return@withContext RegistrationResult.Failure("Device registration failed")
                val secret = devJson.optString("secret").takeIf { it.isNotEmpty() }
                    ?: return@withContext RegistrationResult.Failure("Device registration failed")

                RegistrationResult.Success(
                    SubscriptionEntity(
                        serverUrl = base,
                        userKey = userKey,
                        token = token,
                        deviceId = deviceId,
                        secret = secret,
                    ),
                )
            } catch (e: IOException) {
                RegistrationResult.Failure("Network error: ${e.message ?: "unable to reach server"}")
            } catch (e: Exception) {
                RegistrationResult.Failure("Could not complete registration")
            }
        }

    private fun jsonRequest(url: String, payload: JSONObject): Pair<Int, String> {
        val req = Request.Builder().url(url).post(payload.toString().toRequestBody(JSON)).build()
        return exec(req)
    }

    private fun getRequest(url: String): Pair<Int, String> =
        exec(Request.Builder().url(url).get().build())

    private fun exec(req: Request): Pair<Int, String> =
        client.newCall(req).execute().use { resp -> resp.code to (resp.body?.string().orEmpty()) }

    companion object {
        private val JSON = "application/json; charset=utf-8".toMediaType()

        fun defaultClient(): OkHttpClient = OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(20, TimeUnit.SECONDS)
            .cookieJar(InMemoryCookieJar())
            .build()

        /**
         * Normalizes a user-entered server URL to scheme://host[:port] with no
         * trailing slash. Bare hosts default to https. Returns null for
         * unparseable input, non-http(s) schemes, or a URL with a path/query.
         */
        fun normalizeBase(raw: String): String? {
            val trimmed = raw.trim().trimEnd('/')
            if (trimmed.isEmpty()) return null
            val withScheme = when {
                trimmed.startsWith("http://") || trimmed.startsWith("https://") -> trimmed
                else -> "https://$trimmed"
            }
            val url = withScheme.toHttpUrlOrNull() ?: return null
            if (url.scheme != "http" && url.scheme != "https") return null
            if (url.encodedPath.isNotEmpty() && url.encodedPath != "/") return null
            val defaultPort = if (url.scheme == "https") 443 else 80
            val portSuffix = if (url.port == defaultPort) "" else ":${url.port}"
            return "${url.scheme}://${url.host}$portSuffix"
        }

        private fun statusIsOne(body: String): Boolean =
            runCatching { JSONObject(body).optInt("status", 0) == 1 }.getOrDefault(false)
    }
}

private class InMemoryCookieJar : CookieJar {
    private val cookies = mutableListOf<Cookie>()

    @Synchronized
    override fun saveFromResponse(url: HttpUrl, cookies: List<Cookie>) {
        this.cookies += cookies
    }

    @Synchronized
    override fun loadForRequest(url: HttpUrl): List<Cookie> = cookies.filter { it.matches(url) }
}
