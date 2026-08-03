package net.pushfree.android.up

import android.content.Context
import androidx.room.Room
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import kotlinx.coroutines.runBlocking
import net.pushfree.android.data.AckState
import net.pushfree.android.data.PushFreeDatabase
import net.pushfree.android.data.SubscriptionEntity
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/**
 * Robolectric integration of the UnifiedPush connector dispatch path
 * ([UpDispatcher]) against an in-memory Room store and a [RecordingUpRegistrar].
 *
 * This is the agent-executed MANUAL QA in raw test form:
 *  - happy: MESSAGE_RECEIVED -> MessageEntity persisted (asserted via the DAO)
 *  - failure: UNREGISTERED -> device removed from server-call record
 *  - lifecycle: NEW_ENDPOINT -> record set
 *
 * Fully deterministic: no emulator, no network, no timing (Robolectric provides
 * the Android Context Room needs; the registrar is the network-free recording
 * fake, so no real socket is opened).
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34])
class UnifiedPushTransportTest {

    private lateinit var db: PushFreeDatabase
    private lateinit var registrar: RecordingUpRegistrar
    private lateinit var dispatcher: UpDispatcher
    private val persisted = mutableListOf<net.pushfree.android.data.MessageEntity>()

    @Before
    fun setUp() {
        val context = ApplicationProvider.getApplicationContext<Context>()
        db = Room.inMemoryDatabaseBuilder(context, PushFreeDatabase::class.java)
            .allowMainThreadQueries()
            .build()
        registrar = RecordingUpRegistrar()
        persisted.clear()
        dispatcher = UpDispatcher(
            database = db,
            registrar = registrar,
            onMessagePersisted = { entity -> persisted += entity },
        )
    }

    @After
    fun tearDown() {
        db.close()
    }

    private fun seedSubscription() {
        runBlocking {
            db.subscriptionDao().upsert(
                SubscriptionEntity(
                    serverUrl = SERVER,
                    userKey = "uk",
                    token = "tk",
                    deviceId = "dev",
                    secret = "secret",
                ),
            )
        }
    }

    // happy = MESSAGE_RECEIVED -> entity persisted (asserted via the DAO)
    @Test
    fun message_received_persists_entity_via_dao() = runBlocking {
        seedSubscription()
        val message = UpMessage(
            id = 77L,
            sendId = 770L,
            title = "Build broken",
            body = "production is on fire",
            priority = 2,
            receiptId = "rc77",
            attachmentUri = null,
        )

        dispatcher.dispatch(UpIntent.MessageReceived(message))

        assertEquals(1, db.messageDao().countBySub(SERVER))
        val stored = db.messageDao().getById(77L)
        assertEquals(77L, stored!!.id)
        assertEquals(SERVER, stored.sub)
        assertEquals(770L, stored.sendId)
        assertEquals("Build broken", stored.title)
        assertEquals("production is on fire", stored.body)
        assertEquals(2, stored.priority)
        assertEquals("rc77", stored.receiptId)
        // Emergency (receipt present) seeds PENDING so the ack outbox can fire.
        assertEquals(AckState.PENDING, stored.ackState)
        // The notification hook fired exactly once with the persisted entity.
        assertEquals(listOf(77L), persisted.map { it.id })
    }

    // A non-emergency message maps to NONE ack + null receipt.
    @Test
    fun normal_message_received_persists_none_ack() = runBlocking {
        seedSubscription()
        dispatcher.dispatch(
            UpIntent.MessageReceived(
                UpMessage(id = 8L, sendId = 8L, title = null, body = "hi", priority = 0, receiptId = null, attachmentUri = null),
            ),
        )
        val stored = db.messageDao().getById(8L)!!
        assertNull(stored.receiptId)
        assertEquals(AckState.NONE, stored.ackState)
    }

    // duplicate id (server replay) replaces the row, never creates a second.
    @Test
    fun duplicate_message_id_replaces_row() = runBlocking {
        seedSubscription()
        dispatcher.dispatch(
            UpIntent.MessageReceived(UpMessage(1L, 1L, null, "first", 0, null, null)),
        )
        dispatcher.dispatch(
            UpIntent.MessageReceived(UpMessage(1L, 1L, "T", "second", 2, "r1", null)),
        )
        assertEquals(1, db.messageDao().countBySub(SERVER))
        val stored = db.messageDao().getById(1L)!!
        assertEquals("second", stored.body)
        assertEquals(2, stored.priority)
    }

    // No subscription configured -> message dropped (not orphaned), no crash.
    @Test
    fun message_received_without_subscription_is_dropped() = runBlocking {
        dispatcher.dispatch(
            UpIntent.MessageReceived(UpMessage(1L, 1L, null, "orphan", 0, null, null)),
        )
        assertEquals(0, db.messageDao().countBySub(SERVER))
        assertTrue("no notification posted for a dropped message", persisted.isEmpty())
    }

    // failure = UNREGISTERED -> device removed from server-call record
    @Test
    fun unregistered_removes_device_from_server_call_record() = runBlocking {
        dispatcher.dispatch(UpIntent.NewEndpoint("https://distributor.example/t/abc"))
        assertTrue(registrar.isRegistered())

        dispatcher.dispatch(UpIntent.Unregistered)

        assertFalse(registrar.isRegistered())
        assertNull(registrar.currentEndpoint())
    }

    // NEW_ENDPOINT -> record set (register device with server record)
    @Test
    fun new_endpoint_sets_server_call_record() = runBlocking {
        dispatcher.dispatch(UpIntent.NewEndpoint("https://distributor.example/t/abc"))
        assertTrue(registrar.isRegistered())
        assertEquals("https://distributor.example/t/abc", registrar.currentEndpoint())
    }

    // A corrupt MESSAGE_RECEIVED maps to MessageIgnored -> nothing persisted,
    // no notification, no crash.
    @Test
    fun corrupt_message_is_ignored_without_persisting() = runBlocking {
        seedSubscription()
        dispatcher.dispatch(UpIntent.MessageIgnored)
        assertEquals(0, db.messageDao().countBySub(SERVER))
        assertTrue(persisted.isEmpty())
    }

    // REGISTERED / Unknown are no-ops at the dispatch layer.
    @Test
    fun informational_intents_are_no_ops() = runBlocking {
        seedSubscription()
        dispatcher.dispatch(UpIntent.Registered)
        dispatcher.dispatch(UpIntent.Unknown)
        assertEquals(0, db.messageDao().countBySub(SERVER))
        assertFalse(registrar.isRegistered())
        assertTrue(persisted.isEmpty())
    }

    private companion object {
        const val SERVER = "https://srv.example"
    }
}
