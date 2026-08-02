package net.pushfree.android.data

import androidx.room.Entity
import androidx.room.PrimaryKey

/**
 * Resume point for since-replay per subscription. [lastId] is the highest server
 * message id already persisted for [sub]; reconnecting WS/SSE replays everything
 * strictly after it, then advances the cursor.
 */
@Entity(tableName = "since_cursors")
data class SinceCursor(
    /** Subscription key (matches [SubscriptionEntity.serverUrl]). */
    @PrimaryKey val sub: String,
    /** Highest server message id observed for this subscription. */
    val lastId: Long,
)
