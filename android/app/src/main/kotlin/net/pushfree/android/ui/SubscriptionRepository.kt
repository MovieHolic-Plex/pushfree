package net.pushfree.android.ui

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.flowOf
import kotlinx.coroutines.flow.map
import net.pushfree.android.data.MessageDao
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.data.SubscriptionDao
import net.pushfree.android.data.SubscriptionEntity

/**
 * A server subscription paired with its cached messages, in the shape the
 * subscription-list UI consumes: messages grouped under their owning server.
 */
data class ServerGroup(
    val subscription: SubscriptionEntity,
    val messages: List<MessageEntity>,
)

/**
 * Read-side seam between the Room data layer and the Compose ViewModels.
 * The interface exists so ViewModels can be unit-tested with an in-memory fake
 * (deterministic, no Robolectric) while production wires [RoomSubscriptionRepository].
 */
interface SubscriptionRepository {
    fun observeGroups(): Flow<List<ServerGroup>>
    fun observeMessage(id: Long): Flow<MessageEntity?>
    suspend fun upsert(subscription: SubscriptionEntity)
}

/**
 * Room-backed repository. [observeGroups] reactively follows both the
 * subscription set and each subscription's message cache. [observeMessage]
 * snapshots the row by id.
 */
class RoomSubscriptionRepository(
    private val subscriptions: SubscriptionDao,
    private val messages: MessageDao,
) : SubscriptionRepository {

    @OptIn(ExperimentalCoroutinesApi::class)
    override fun observeGroups(): Flow<List<ServerGroup>> =
        subscriptions.observeAll().flatMapLatest { subs ->
            if (subs.isEmpty()) {
                flowOf(emptyList())
            } else {
                combine(
                    flows = subs.map { sub ->
                        messages.observeBySub(sub.serverUrl).map { msgs -> sub to msgs }
                    },
                ) { pairs ->
                    pairs.map { (sub, msgs) -> ServerGroup(sub, msgs) }
                        .sortedBy { it.subscription.serverUrl }
                }
            }
        }

    override fun observeMessage(id: Long): Flow<MessageEntity?> =
        kotlinx.coroutines.flow.flow { emit(messages.getById(id)) }

    override suspend fun upsert(subscription: SubscriptionEntity) {
        subscriptions.upsert(subscription)
    }
}
