package net.pushfree.android.outbox

import android.content.Context
import androidx.room.Room
import net.pushfree.android.data.PushFreeDatabase

/**
 * Process-wide service locator for the ack outbox.
 *
 * Production lazily builds a single [PushFreeDatabase] (file-backed) and uses the
 * real [HttpUrlConnectionAckPoster]. Tests override both via [setDatabase] and
 * [poster] for deterministic, socket-free runs (in-memory Room + a fake poster).
 *
 * Holding the DB singleton here (rather than re-opening per worker run) keeps
 * in-memory test databases observable across the test -> worker boundary.
 */
object AckOutboxServices {
    @Volatile
    private var instance: PushFreeDatabase? = null

    /** HTTP poster used by [AckWorker]. Tests assign a fake. */
    @Volatile
    var poster: AckPoster = HttpUrlConnectionAckPoster

    fun database(context: Context): PushFreeDatabase =
        instance ?: synchronized(this) {
            instance ?: Room.databaseBuilder(
                context.applicationContext,
                PushFreeDatabase::class.java,
                PushFreeDatabase.NAME,
            ).fallbackToDestructiveMigration().build().also { instance = it }
        }

    /** Test seam: install an in-memory or fake database (null resets to lazy). */
    fun setDatabase(db: PushFreeDatabase?) {
        synchronized(this) { instance = db }
    }
}
