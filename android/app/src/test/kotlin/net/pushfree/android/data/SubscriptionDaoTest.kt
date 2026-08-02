package net.pushfree.android.data

import android.content.Context
import androidx.room.Room
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import kotlinx.coroutines.runBlocking
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/**
 * In-memory Room DAO tests. Robolectric is required because
 * [Room.inMemoryDatabaseBuilder] needs a real Android [Context] and the framework
 * SQLite, neither of which exist on a plain JVM host.
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34])
class SubscriptionDaoTest {

    private lateinit var db: PushFreeDatabase
    private lateinit var dao: SubscriptionDao

    private fun sub(url: String) = SubscriptionEntity(
        serverUrl = url,
        userKey = "key-$url",
        token = "token-$url",
        deviceId = "dev-$url",
        secret = "secret-$url",
    )

    @Before
    fun setUp() {
        val context = ApplicationProvider.getApplicationContext<Context>()
        db = Room.inMemoryDatabaseBuilder(context, PushFreeDatabase::class.java)
            .allowMainThreadQueries()
            .build()
        dao = db.subscriptionDao()
    }

    @After
    fun tearDown() {
        db.close()
    }

    @Test
    fun insert_then_query_returns_row() = runBlocking {
        dao.upsert(sub("https://srv.example"))

        val all = dao.getAll()
        assertEquals(1, all.size)
        assertEquals("https://srv.example", all[0].serverUrl)
        assertEquals("key-https://srv.example", all[0].userKey)
    }

    @Test
    fun getByServerUrl_returns_null_when_absent() = runBlocking {
        assertNull(dao.getByServerUrl("https://nope"))
    }

    @Test
    fun upsert_replaces_existing_subscription() = runBlocking {
        dao.upsert(sub("https://srv.example"))
        dao.upsert(sub("https://srv.example").copy(token = "token-rotated"))

        assertEquals("one subscription per server, not two", 1, dao.getAll().size)
        val stored = dao.getByServerUrl("https://srv.example")
        assertNotNull(stored)
        assertEquals("token-rotated", stored!!.token)
    }

    @Test
    fun deleteByServerUrl_removes_row() = runBlocking {
        dao.upsert(sub("https://srv.example"))
        dao.deleteByServerUrl("https://srv.example")

        assertEquals(0, dao.getAll().size)
    }
}
