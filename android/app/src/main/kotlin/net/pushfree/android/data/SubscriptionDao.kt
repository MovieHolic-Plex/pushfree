package net.pushfree.android.data

import androidx.room.Dao
import androidx.room.Insert
import androidx.room.OnConflictStrategy
import androidx.room.Query
import kotlinx.coroutines.flow.Flow

@Dao
interface SubscriptionDao {
    @Insert(onConflict = OnConflictStrategy.REPLACE)
    suspend fun upsert(subscription: SubscriptionEntity)

    @Query("SELECT * FROM subscriptions ORDER BY serverUrl")
    fun observeAll(): Flow<List<SubscriptionEntity>>

    @Query("SELECT * FROM subscriptions ORDER BY serverUrl")
    suspend fun getAll(): List<SubscriptionEntity>

    @Query("SELECT * FROM subscriptions WHERE serverUrl = :serverUrl")
    suspend fun getByServerUrl(serverUrl: String): SubscriptionEntity?

    @Query("DELETE FROM subscriptions WHERE serverUrl = :serverUrl")
    suspend fun deleteByServerUrl(serverUrl: String)
}
