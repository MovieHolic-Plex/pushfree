package net.pushfree.android.ui

import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.flow
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.data.SubscriptionEntity

/** In-memory repository fake for ViewModel tests (no Robolectric, fully deterministic). */
class FakeSubscriptionRepository(
    initialGroups: List<ServerGroup> = emptyList(),
) : SubscriptionRepository {
    private val groupsState = MutableStateFlow(initialGroups)
    private val messageUpdates = MutableSharedFlow<MessageEntity?>(replay = 1)
    val upserted = mutableListOf<SubscriptionEntity>()

    fun emitGroups(value: List<ServerGroup>) {
        groupsState.value = value
    }

    fun emitMessage(message: MessageEntity?) {
        messageUpdates.tryEmit(message)
    }

    override fun observeGroups(): Flow<List<ServerGroup>> = groupsState.asStateFlow()

    override fun observeMessage(id: Long): Flow<MessageEntity?> = flow {
        emit(messageUpdates.replayCache.lastOrNull())
        messageUpdates.asSharedFlow().collect { emit(it) }
    }

    override suspend fun upsert(subscription: SubscriptionEntity) {
        upserted += subscription
    }
}
