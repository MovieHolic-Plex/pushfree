package net.pushfree.android.ui.settings

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.setMain
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

class SettingsViewModelTest {

    private val dispatcher = UnconfinedTestDispatcher()

    @Before
    fun setUp() {
        Dispatchers.setMain(dispatcher)
    }

    @After
    fun tearDown() {
        Dispatchers.resetMain()
    }

    @Test
    fun `select transport persists through the store`() = runTest(dispatcher) {
        val store = FakeTransportPreferenceStore()
        val vm = SettingsViewModel(store, RecordingNotificationTester())
        val job = launch(dispatcher) { vm.state.toList(mutableListOf()) }
        vm.selectTransport(TransportPreference.FCM)
        assertEquals(TransportPreference.FCM, store.set)
        job.cancel()
    }

    @Test
    fun `host permission flags are reflected in state`() = runTest(dispatcher) {
        val vm = SettingsViewModel(FakeTransportPreferenceStore(), RecordingNotificationTester())
        val states = mutableListOf<SettingsUiState>()
        val job = launch(dispatcher) { vm.state.toList(states) }

        vm.setPermissionFlags(postGranted = false, batteryExempt = false, fsiGranted = true)
        val latest = states.last()
        assertEquals(false, latest.postNotificationsGranted)
        assertEquals(false, latest.batteryOptimizationExempt)
        assertEquals(true, latest.fullScreenIntentGranted)
        assertEquals("Grant notification permission to display alerts.", latest.postNotifications.guidance)
        job.cancel()
    }

    @Test
    fun `post test notification invokes tester and publishes result`() = runTest(dispatcher) {
        val tester = RecordingNotificationTester(TestNotificationResult.FAILED)
        val vm = SettingsViewModel(FakeTransportPreferenceStore(), tester)
        val states = mutableListOf<SettingsUiState>()
        val job = launch(dispatcher) { vm.state.toList(states) }

        vm.postTestNotification()
        assertEquals(1, tester.callCount)
        assertTrue("result surfaced in state", states.any { it.testResult == TestNotificationResult.FAILED })

        vm.consumeTestResult()
        assertNull("result cleared after consume", states.last().testResult)
        job.cancel()
    }
}
