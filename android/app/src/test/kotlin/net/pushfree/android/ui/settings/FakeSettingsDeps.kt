package net.pushfree.android.ui.settings

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow

/** In-memory transport store fake for SettingsViewModel tests. */
class FakeTransportPreferenceStore(initial: TransportPreference = TransportPreference.WEBSOCKET) :
    TransportPreferenceStore {
    private val state = MutableStateFlow(initial)
    val set: TransportPreference? get() = state.value

    override fun observe() = state.asStateFlow()
    override fun set(value: TransportPreference) {
        state.value = value
    }
}

/** Recording tester fake: captures the call and returns the configured result. */
class RecordingNotificationTester(private val result: TestNotificationResult = TestNotificationResult.OK) :
    NotificationTester {
    var callCount = 0
        private set
    override fun postTestNotification(): TestNotificationResult {
        callCount++
        return result
    }
}
