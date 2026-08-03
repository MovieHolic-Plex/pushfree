package net.pushfree.android.ui.detail

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import net.pushfree.android.data.AckState
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.ui.SubscriptionRepository

/** View state for the message detail screen. */
data class MessageDetailUiState(
    val message: MessageEntity? = null,
) {
    val title: String? get() = message?.title
    val body: String get() = message?.body.orEmpty()
    val priority: Int get() = message?.priority ?: 0
    val attachmentUri: String? get() = message?.attachmentUri
    val receiptId: String? get() = message?.receiptId
    val ackState: AckState get() = message?.ackState ?: AckState.NONE
    val isEmergency: Boolean get() = priority >= 2
    val canAcknowledge: Boolean get() = receiptId != null && ackState == AckState.PENDING
}

class MessageDetailViewModel(
    repository: SubscriptionRepository,
    messageId: Long,
) : ViewModel() {
    val state: StateFlow<MessageDetailUiState> =
        repository.observeMessage(messageId)
            .map { MessageDetailUiState(it) }
            .stateIn(
                scope = viewModelScope,
                started = SharingStarted.WhileSubscribed(STOP_TIMEOUT_MS),
                initialValue = MessageDetailUiState(),
            )

    private companion object {
        const val STOP_TIMEOUT_MS = 5_000L
    }
}
