package net.pushfree.android.data

import android.content.Context
import androidx.room.Room
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import kotlinx.coroutines.runBlocking
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

@RunWith(AndroidJUnit4::class)
@Config(sdk = [34])
class MessageDaoTest {

    private lateinit var db: PushFreeDatabase
    private lateinit var dao: MessageDao

    private fun msg(id: Long, sub: String, body: String, priority: Int = 0) = MessageEntity(
        id = id,
        sub = sub,
        sendId = 100L,
        title = null,
        body = body,
        priority = priority,
        attachmentUri = null,
        ackState = AckState.NONE,
        receiptId = null,
    )

    @Before
    fun setUp() {
        val context = ApplicationProvider.getApplicationContext<Context>()
        db = Room.inMemoryDatabaseBuilder(context, PushFreeDatabase::class.java)
            .allowMainThreadQueries()
            .build()
        dao = db.messageDao()
    }

    @After
    fun tearDown() {
        db.close()
    }

    @Test
    fun insert_then_query_by_sub() = runBlocking {
        dao.insert(msg(1L, "https://srv", "hello"))

        val rows = dao.getBySub("https://srv")
        assertEquals(1, rows.size)
        assertEquals("hello", rows[0].body)
    }

    @Test
    fun duplicate_id_replaces_row_with_latest_values() = runBlocking {
        dao.insert(msg(1L, "https://srv", "first"))
        dao.insert(msg(1L, "https://srv", "second", priority = 2))

        assertEquals("duplicate id must not create a second row", 1, dao.countBySub("https://srv"))
        val stored = dao.getById(1L)
        assertNotNull(stored)
        assertEquals("second", stored!!.body)
        assertEquals(2, stored.priority)
    }

    @Test
    fun getBySub_orders_by_id_ascending() = runBlocking {
        // Inserted out of order; result must be sorted by id.
        dao.insert(msg(3L, "https://srv", "c"))
        dao.insert(msg(1L, "https://srv", "a"))
        dao.insert(msg(2L, "https://srv", "b"))

        val rows = dao.getBySub("https://srv")
        assertEquals(listOf(1L, 2L, 3L), rows.map { it.id })
        assertEquals(listOf("a", "b", "c"), rows.map { it.body })
    }

    @Test
    fun getBySub_is_scoped_to_subscription() = runBlocking {
        dao.insert(msg(1L, "https://a", "a1"))
        dao.insert(msg(2L, "https://b", "b2"))

        assertEquals(1, dao.countBySub("https://a"))
        assertEquals(1, dao.countBySub("https://b"))
    }

    @Test
    fun updateAckState_persists_new_state() = runBlocking {
        dao.insert(msg(1L, "https://srv", "boom", priority = 2)
            .copy(ackState = AckState.PENDING, receiptId = "r1"))
        dao.updateAckState(1L, AckState.ACKED)

        assertEquals(AckState.ACKED, dao.getById(1L)!!.ackState)
    }
}
