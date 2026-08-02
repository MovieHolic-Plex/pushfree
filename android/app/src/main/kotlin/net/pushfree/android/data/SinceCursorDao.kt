package net.pushfree.android.data

import androidx.room.Dao
import androidx.room.Insert
import androidx.room.OnConflictStrategy
import androidx.room.Query

@Dao
interface SinceCursorDao {
    @Insert(onConflict = OnConflictStrategy.REPLACE)
    suspend fun upsert(cursor: SinceCursor)

    @Query("SELECT lastId FROM since_cursors WHERE sub = :sub")
    suspend fun getLastId(sub: String): Long?

    @Query("SELECT * FROM since_cursors WHERE sub = :sub")
    suspend fun get(sub: String): SinceCursor?
}
