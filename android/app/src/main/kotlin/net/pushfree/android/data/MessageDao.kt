package net.pushfree.android.data

import androidx.room.Dao
import androidx.room.Insert
import androidx.room.OnConflictStrategy
import androidx.room.Query
import kotlinx.coroutines.flow.Flow

@Dao
interface MessageDao {
    /** Insert (or replace on duplicate server id) a message. */
    @Insert(onConflict = OnConflictStrategy.REPLACE)
    suspend fun insert(message: MessageEntity)

    @Query("SELECT * FROM messages WHERE sub = :sub ORDER BY id ASC")
    fun observeBySub(sub: String): Flow<List<MessageEntity>>

    @Query("SELECT * FROM messages WHERE sub = :sub ORDER BY id ASC")
    suspend fun getBySub(sub: String): List<MessageEntity>

    @Query("SELECT * FROM messages WHERE id = :id")
    suspend fun getById(id: Long): MessageEntity?

    @Query("SELECT COUNT(*) FROM messages WHERE sub = :sub")
    suspend fun countBySub(sub: String): Int

    @Query("UPDATE messages SET ackState = :state WHERE id = :id")
    suspend fun updateAckState(id: Long, state: AckState)
}
