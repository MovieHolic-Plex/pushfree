package net.pushfree.android.ui.screenshots

import app.cash.paparazzi.Paparazzi
import androidx.compose.material3.SnackbarHostState
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import net.pushfree.android.data.AckState
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.data.SubscriptionEntity
import net.pushfree.android.ui.ServerGroup
import net.pushfree.android.ui.detail.MessageDetailContent
import net.pushfree.android.ui.detail.MessageDetailUiState
import net.pushfree.android.ui.onboarding.AddServerContent
import net.pushfree.android.ui.onboarding.AddServerUiState
import net.pushfree.android.ui.settings.SettingsContent
import net.pushfree.android.ui.settings.SettingsUiState
import net.pushfree.android.ui.settings.TransportPreference
import net.pushfree.android.ui.subscription.SubscriptionListContent
import net.pushfree.android.ui.subscription.SubscriptionListUiState
import net.pushfree.android.ui.theme.PushfreeTheme
import org.junit.Rule
import org.junit.Test

/**
 * Paparazzi golden screenshots for the base Compose screens (todo 34).
 *
 * Dynamic color is forced off so the palette is stable across hosts; the
 * Material3 baseline theme renders deterministically under layoutlib. Record
 * once with `recordPaparazziDebug`; `verifyPaparazziDebug` compares thereafter.
 */
class PaparazziScreenshots {

    @get:Rule
    val paparazzi = Paparazzi()

    private fun sub(url: String) = SubscriptionEntity(
        serverUrl = url,
        userKey = "USERKEYUSERKEYUSERKEYUS",
        token = "APPTOKENAPPTOKENAPPTOKENAPP",
        deviceId = "DEVDEVDEVDEVDEVDEVDEVDEVDEV",
        secret = "SECRETSECRETSECRETSECRETSE",
    )

    private fun msg(id: Long, sub: String, title: String, body: String, priority: Int) =
        MessageEntity(
            id = id,
            sub = sub,
            sendId = id,
            title = title,
            body = body,
            priority = priority,
            attachmentUri = null,
            ackState = AckState.NONE,
            receiptId = null,
        )

    /** 1. Subscription list grouped by server. */
    @Test
    fun subscriptionList() {
        paparazzi.snapshot("subscription_list") {
            PushfreeTheme(dynamicColor = false) {
                SubscriptionListContent(
                    state = SubscriptionListUiState(
                        groups = listOf(
                            ServerGroup(
                                sub("https://push.example.com"),
                                listOf(
                                    msg(1, "https://push.example.com", "Build green", "All checks passed on main.", 1),
                                    msg(2, "https://push.example.com", "Deploy done", "v0.4.2 rolled out to prod.", 0),
                                ),
                            ),
                            ServerGroup(
                                sub("https://alerts.home.lan"),
                                listOf(
                                    msg(3, "https://alerts.home.lan", "Doorbell", "Motion at the front door.", 0),
                                ),
                            ),
                        ),
                    ),
                    onMessageClick = {},
                    onAddServerClick = {},
                    onSettingsClick = {},
                )
            }
        }
    }

    /** 2. Add-server onboarding form (happy path golden). */
    @Test
    fun addServer() {
        paparazzi.snapshot("add_server") {
            PushfreeTheme(dynamicColor = false) {
                AddServerContent(
                    state = AddServerUiState(
                        serverUrl = "https://push.example.com",
                        email = "user@example.com",
                    ),
                    snackbarHostState = SnackbarHostState(),
                    onServerUrlChange = {},
                    onEmailChange = {},
                    onPasswordChange = {},
                    onDeviceNameChange = {},
                    onSubmit = {},
                    onBack = {},
                )
            }
        }
    }

    /** 3. Settings screen. */
    @Test
    fun settings() {
        paparazzi.snapshot("settings") {
            PushfreeTheme(dynamicColor = false) {
                SettingsContent(
                    state = SettingsUiState(
                        transport = TransportPreference.WEBSOCKET,
                        batteryOptimizationExempt = false,
                        fullScreenIntentGranted = false,
                        postNotificationsGranted = true,
                    ),
                    snackbarHostState = SnackbarHostState(),
                    onBack = {},
                    onSelectTransport = {},
                    onPostTest = {},
                    onOpenBatterySettings = {},
                    onOpenFullScreenIntentSettings = {},
                    onOpenNotificationSettings = {},
                )
            }
        }
    }

    /** 4. Add-server failure: invalid URL surfaced as an error snackbar. */
    @Test
    fun addServerError() {
        val snackbarHostState = SnackbarHostState()
        // Populate the snackbar synchronously: an unconfined coroutine sets the
        // host's current snackbar data before showSnackbar() suspends on dismissal.
        val scope = CoroutineScope(Dispatchers.Unconfined)
        scope.launch { snackbarHostState.showSnackbar("Invalid server URL") }
        paparazzi.snapshot("add_server_error") {
            PushfreeTheme(dynamicColor = false) {
                AddServerContent(
                    state = AddServerUiState(
                        serverUrl = "not a url !!",
                        error = "Invalid server URL",
                    ),
                    snackbarHostState = snackbarHostState,
                    onServerUrlChange = {},
                    onEmailChange = {},
                    onPasswordChange = {},
                    onDeviceNameChange = {},
                    onSubmit = {},
                    onBack = {},
                )
            }
        }
    }
}
