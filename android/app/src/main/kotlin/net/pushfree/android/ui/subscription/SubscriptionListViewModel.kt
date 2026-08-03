package net.pushfree.android.ui.subscription

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.data.SubscriptionEntity
import net.pushfree.android.ui.ServerGroup
import net.pushfree.android.ui.SubscriptionRepository

/** Immutable view state for the subscription-list screen. */
data class SubscriptionListUiState(
    val groups: List<ServerGroup> = emptyList(),
) {
    val isEmpty: Boolean get() = groups.isEmpty()
    val messageCount: Int get() = groups.sumOf { it.messages.size }
}

class SubscriptionListViewModel(
    repository: SubscriptionRepository,
) : ViewModel() {
    val state: StateFlow<SubscriptionListUiState> =
        repository.observeGroups()
            .map { SubscriptionListUiState(groups = it) }
            .stateIn(
                scope = viewModelScope,
                started = SharingStarted.WhileSubscribed(STOP_TIMEOUT_MS),
                initialValue = SubscriptionListUiState(),
            )

    private companion object {
        const val STOP_TIMEOUT_MS = 5_000L
    }
}

sealed interface MessageRow {
    val id: Long
}

data class ServerHeaderRow(val subscription: SubscriptionEntity) : MessageRow {
    override val id: Long get() = subscription.serverUrl.hashCode().toLong()
}

data class MessageItemRow(val message: MessageEntity) : MessageRow {
    override val id: Long get() = message.id
}

fun flattenGroups(groups: List<ServerGroup>): List<MessageRow> =
    buildList {
        for (group in groups) {
            add(ServerHeaderRow(group.subscription))
            addAll(group.messages.map(::MessageItemRow))
        }
    }
