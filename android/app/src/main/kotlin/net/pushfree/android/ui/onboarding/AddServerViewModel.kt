package net.pushfree.android.ui.onboarding

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import net.pushfree.android.ui.SubscriptionRepository

/** Editable onboarding form + the async registration outcome. */
data class AddServerUiState(
    val serverUrl: String = "",
    val email: String = "",
    val password: String = "",
    val deviceName: String = "",
    val isLoading: Boolean = false,
    val error: String? = null,
    val success: Boolean = false,
) {
    val canSubmit: Boolean
        get() = !isLoading && serverUrl.isNotBlank() && email.isNotBlank() && password.isNotBlank()
}

class AddServerViewModel(
    private val registrar: DeviceRegistrar,
    private val repository: SubscriptionRepository,
) : ViewModel() {

    private val _state = MutableStateFlow(AddServerUiState())
    val state: StateFlow<AddServerUiState> = _state.asStateFlow()

    fun onServerUrlChange(value: String) = _state.update { it.copy(serverUrl = value) }
    fun onEmailChange(value: String) = _state.update { it.copy(email = value) }
    fun onPasswordChange(value: String) = _state.update { it.copy(password = value) }
    fun onDeviceNameChange(value: String) = _state.update { it.copy(deviceName = value) }

    fun consumeError() = _state.update { it.copy(error = null) }

    fun reset() {
        _state.value = AddServerUiState()
    }

    fun register() {
        val current = _state.value
        if (!current.canSubmit) return
        _state.update { it.copy(isLoading = true, error = null) }
        viewModelScope.launch {
            when (val result = registrar.register(current.toInput())) {
                is RegistrationResult.Success -> {
                    repository.upsert(result.subscription)
                    _state.update { it.copy(isLoading = false, success = true) }
                }
                is RegistrationResult.Failure -> {
                    _state.update { it.copy(isLoading = false, error = result.reason) }
                }
            }
        }
    }

    private fun AddServerUiState.toInput() = RegistrationInput(
        serverUrl = serverUrl,
        email = email,
        password = password,
        deviceName = deviceName,
    )
}
