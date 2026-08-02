package net.pushfree.android.data

import android.content.Context
import androidx.room.Room
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import kotlinx.coroutines.runBlocking
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

@RunWith(AndroidJUnit4::class)
@Config(sdk = [34])
class SinceCursorDaoTest {

    private lateinit var db: PushFreeDatabase
    private lateinit var dao: SinceCursorDao

    @Before
    fun setUp() {
        val context = ApplicationProvider.getApplicationContext<Context>()
        db = Room.inMemoryDatabaseBuilder(context, PushFreeDatabase::class.java)
            .allowMainThreadQueries()
            .build()
        dao = db.sinceCursorDao()
    }

    @After
    fun tearDown() {
        db.close()
    }

    @Test
    fun getLastId_returns_null_when_absent() = runBlocking {
        assertNull(dao.getLastId("https://srv"))
    }

    @Test
    fun upsert_then_read_returns_last_id() = runBlocking {
        dao.upsert(SinceCursor("https://srv", 42L))

        assertEquals(42L, dao.getLastId("https://srv"))
    }

    @Test
    fun upsert_replaces_advancing_cursor() = runBlocking {
        dao.upsert(SinceCursor("https://srv", 10L))
        dao.upsert(SinceCursor("https://srv", 99L))

        assertEquals("cursor must advance, not duplicate", 99L, dao.getLastId("https://srv"))
    }
}
