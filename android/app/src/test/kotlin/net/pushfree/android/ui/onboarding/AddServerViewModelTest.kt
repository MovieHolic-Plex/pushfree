package net.pushfree.android.ui.onboarding

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.setMain
import net.pushfree.android.data.SubscriptionEntity
import net.pushfree.android.ui.FakeSubscriptionRepository
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

class AddServerViewModelTest {

    private val dispatcher = UnconfinedTestDispatcher()

    @Before
    fun setUp() {
        Dispatchers.setMain(dispatcher)
    }

    @After
    fun tearDown() {
        Dispatchers.resetMain()
    }

    private fun okRegistrar(url: String) = DeviceRegistrar {
        RegistrationResult.Success(
            SubscriptionEntity(
                serverUrl = url,
                userKey = "USERKEYUSERKEYUSERKEYUS",
                token = "APPTOKENAPPTOKENAPPTOKENAPP",
                deviceId = "DEVDEVDEVDEVDEVDEVDEVDEVDEV",
                secret = "SECRETSECRETSECRETSECRETSE",
            ),
        )
    }

    private fun formFilled(vm: AddServerViewModel, url: String = "https://srv") {
        vm.onServerUrlChange(url)
        vm.onEmailChange("user@example.com")
        vm.onPasswordChange("hunter2")
    }

    @Test
    fun `success path persists subscription and signals success`() = runTest(dispatcher) {
        val repo = FakeSubscriptionRepository()
        val vm = AddServerViewModel(okRegistrar("https://srv"), repo)
        formFilled(vm)
        vm.register()

        assertTrue(vm.state.value.success)
        assertFalse(vm.state.value.isLoading)
        assertEquals(1, repo.upserted.size)
        assertEquals("https://srv", repo.upserted.first().serverUrl)
    }

    @Test
    fun `invalid url sets error for the snackbar`() = runTest(dispatcher) {
        val repo = FakeSubscriptionRepository()
        val vm = AddServerViewModel(
            registrar = DeviceRegistrar { RegistrationResult.Failure("Invalid server URL") },
            repository = repo,
        )
        formFilled(vm, url = "not a url !!")
        vm.register()

        assertEquals("Invalid server URL", vm.state.value.error)
        assertFalse(vm.state.value.success)
        assertTrue("nothing persisted on failure", repo.upserted.isEmpty())

        vm.consumeError()
        assertNull(vm.state.value.error)
    }

    @Test
    fun `cannot submit without required fields`() {
        val vm = AddServerViewModel(okRegistrar("x"), FakeSubscriptionRepository())
        assertFalse("empty form blocked", vm.state.value.canSubmit)

        vm.onServerUrlChange("https://srv")
        assertFalse("missing email+password blocked", vm.state.value.canSubmit)

        vm.onEmailChange("a@b.com")
        vm.onPasswordChange("pw")
        assertTrue("complete form enabled", vm.state.value.canSubmit)
    }

    @Test
    fun `loading flag is reset after success`() = runTest(dispatcher) {
        val vm = AddServerViewModel(okRegistrar("https://srv"), FakeSubscriptionRepository())
        formFilled(vm)
        vm.register()
        assertFalse(vm.state.value.isLoading)
        assertTrue(vm.state.value.success)
    }
}
