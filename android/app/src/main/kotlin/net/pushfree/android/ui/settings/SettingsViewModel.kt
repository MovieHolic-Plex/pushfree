package net.pushfree.android.ui.settings

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** One row of the settings status panel (permission/system state + guidance). */
data class PermissionStatus(
    val label: String,
    val granted: Boolean,
    /** Short guidance shown when [granted] is false. */
    val guidance: String,
)

/**
 * View state for the settings screen. Permission flags are pushed in by the host
 * (read from the real system APIs) because they require a live Context; the
 * ViewModel owns the transport preference and the test-notification outcome.
 */
data class SettingsUiState(
    val transport: TransportPreference = TransportPreference.WEBSOCKET,
    val batteryOptimizationExempt: Boolean = true,
    val fullScreenIntentGranted: Boolean = true,
    val postNotificationsGranted: Boolean = true,
    val testResult: TestNotificationResult? = null,
) {
    val postNotifications: PermissionStatus
        get() = PermissionStatus(
            label = "Notifications",
            granted = postNotificationsGranted,
            guidance = "Grant notification permission to display alerts.",
        )
    val battery: PermissionStatus
        get() = PermissionStatus(
            label = "Battery optimization",
            granted = batteryOptimizationExempt,
            guidance = "Exempt the app from battery optimization so the WebSocket stays alive.",
        )
    val fullScreenIntent: PermissionStatus
        get() = PermissionStatus(
            label = "Full-screen intents",
            granted = fullScreenIntentGranted,
            guidance = "Allow full-screen notifications so emergency alerts ring over the lock screen (Android 14+).",
        )
}

class SettingsViewModel(
    private val transportStore: TransportPreferenceStore,
    private val tester: NotificationTester,
) : ViewModel() {

    /** Permission flags the host pushes in (they need a live Context). */
    private val permissions = MutableStateFlow(PermissionFlags())

    val state: StateFlow<SettingsUiState> =
        combine(transportStore.observe(), permissions) { transport, perms ->
            SettingsUiState(
                transport = transport,
                batteryOptimizationExempt = perms.batteryExempt,
                fullScreenIntentGranted = perms.fsiGranted,
                postNotificationsGranted = perms.postGranted,
                testResult = perms.testResult,
            )
        }.stateIn(
            scope = viewModelScope,
            started = SharingStarted.WhileSubscribed(STOP_TIMEOUT_MS),
            initialValue = SettingsUiState(),
        )

    /** Host calls this whenever permission/system state is re-read. */
    fun setPermissionFlags(
        postGranted: Boolean,
        batteryExempt: Boolean,
        fsiGranted: Boolean,
    ) {
        permissions.value = permissions.value.copy(
            postGranted = postGranted,
            batteryExempt = batteryExempt,
            fsiGranted = fsiGranted,
        )
    }

    fun selectTransport(value: TransportPreference) {
        viewModelScope.launch { transportStore.set(value) }
    }

    /** Fire the test notification and publish the outcome. */
    fun postTestNotification() {
        val result = tester.postTestNotification()
        permissions.update { it.copy(testResult = result) }
    }

    /** Clear the displayed test result once the host has shown it. */
    fun consumeTestResult() {
        permissions.update { it.copy(testResult = null) }
    }

    private data class PermissionFlags(
        val postGranted: Boolean = true,
        val batteryExempt: Boolean = true,
        val fsiGranted: Boolean = true,
        val testResult: TestNotificationResult? = null,
    )

    private companion object {
        const val STOP_TIMEOUT_MS = 5_000L
    }
}
