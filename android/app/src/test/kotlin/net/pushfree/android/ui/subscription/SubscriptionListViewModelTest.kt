package net.pushfree.android.ui.subscription

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.setMain
import net.pushfree.android.data.AckState
import net.pushfree.android.data.MessageEntity
import net.pushfree.android.data.SubscriptionEntity
import net.pushfree.android.ui.FakeSubscriptionRepository
import net.pushfree.android.ui.ServerGroup
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

class SubscriptionListViewModelTest {

    private val dispatcher = UnconfinedTestDispatcher()

    @Before
    fun setUp() {
        Dispatchers.setMain(dispatcher)
    }

    @After
    fun tearDown() {
        Dispatchers.resetMain()
    }

    private fun sub(url: String) = SubscriptionEntity(
        serverUrl = url,
        userKey = "USERKEYUSERKEYUSERKEYUS",
        token = "APPTOKENAPPTOKENAPPTOKENAPP",
        deviceId = "DEVDEVDEVDEVDEVDEVDEVDEVDEV",
        secret = "SECRETSECRETSECRETSECRETSE",
    )

    private fun msg(id: Long, sub: String, priority: Int = 0) = MessageEntity(
        id = id,
        sub = sub,
        sendId = id,
        title = "Title $id",
        body = "Body $id",
        priority = priority,
        attachmentUri = null,
        ackState = AckState.NONE,
        receiptId = null,
    )

    @Test
    fun `emits empty state then grouped servers`() = runTest(dispatcher) {
        val repo = FakeSubscriptionRepository()
        val vm = SubscriptionListViewModel(repo)
        val collected = mutableListOf<SubscriptionListUiState>()
        val job = launch(dispatcher) { vm.state.toList(collected) }

        repo.emitGroups(
            listOf(
                ServerGroup(sub("https://a"), listOf(msg(1, "https://a"), msg(2, "https://a"))),
                ServerGroup(sub("https://b"), listOf(msg(3, "https://b"))),
            ),
        )

        assertTrue(collected.any { it.isEmpty }) // initial value
        val populated = collected.last { !it.isEmpty }
        assertEquals(2, populated.groups.size)
        assertEquals(3, populated.messageCount)
        job.cancel()
    }

    @Test
    fun `flatten produces header followed by its messages`() {
        val rows = flattenGroups(
            listOf(ServerGroup(sub("https://a"), listOf(msg(1, "https://a"), msg(2, "https://a")))),
        )
        assertEquals(3, rows.size)
        assertTrue("first row is a server header", rows[0] is ServerHeaderRow)
        assertTrue(rows[1] is MessageItemRow)
        assertTrue(rows[2] is MessageItemRow)
    }

    @Test
    fun `empty groups produce no rows`() {
        assertTrue(flattenGroups(emptyList()).isEmpty())
    }
}
