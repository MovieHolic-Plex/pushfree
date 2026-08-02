package net.pushfree.android.data

import androidx.room.Database
import androidx.room.RoomDatabase
import androidx.room.TypeConverters

/**
 * Local Room store for the Android client. Holds the configured server
 * subscriptions, the per-subscription message cache (deduped by server id), and
 * the since-cursor used to resume WS/SSE replay after a reconnect.
 */
@Database(
    entities = [
        SubscriptionEntity::class,
        MessageEntity::class,
        SinceCursor::class,
    ],
    version = 1,
    exportSchema = false,
)
@TypeConverters(Converters::class)
abstract class PushFreeDatabase : RoomDatabase() {
    abstract fun subscriptionDao(): SubscriptionDao
    abstract fun messageDao(): MessageDao
    abstract fun sinceCursorDao(): SinceCursorDao

    companion object {
        const val NAME = "pushfree.db"
    }
}
