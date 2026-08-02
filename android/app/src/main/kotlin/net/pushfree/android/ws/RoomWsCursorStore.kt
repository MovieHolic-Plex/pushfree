package net.pushfree.android.ws

import net.pushfree.android.data.SinceCursor
import net.pushfree.android.data.SinceCursorDao

/**
 * Room-backed [WsCursorStore]: persists the per-subscription since-cursor so a
 * reconnect replays only messages strictly after the last id observed.
 *
 * [sub] is the subscription key (matches [net.pushfree.android.data.SubscriptionEntity.serverUrl]).
 */
class RoomWsCursorStore(
    private val dao: SinceCursorDao,
    private val sub: String,
) : WsCursorStore {
    override suspend fun read(): Long? = dao.getLastId(sub)
    override suspend fun write(id: Long) = dao.upsert(SinceCursor(sub, id))
}
