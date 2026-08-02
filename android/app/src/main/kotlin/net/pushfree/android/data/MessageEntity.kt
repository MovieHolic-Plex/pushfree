package net.pushfree.android.data

import androidx.room.Entity
import androidx.room.PrimaryKey

/**
 * A delivered message for a subscription. [id] is the server-assigned message id;
 * inserting a duplicate id replaces the row (OnConflictStrategy.REPLACE on the DAO),
 * so the store never holds two rows for the same server message.
 */
@Entity(tableName = "messages")
data class MessageEntity(
    /** Server message id; primary key. Duplicate inserts replace in place. */
    @PrimaryKey val id: Long,
    /** Owning subscription (matches [SubscriptionEntity.serverUrl]). */
    val sub: String,
    /** Parent send id on the server (groups fan-out messages). */
    val sendId: Long,
    val title: String?,
    val body: String,
    /** Pushover priority -2..2. */
    val priority: Int,
    /** Local/content URI of a downloaded attachment, if any. */
    val attachmentUri: String?,
    /** Ack lifecycle state for emergency (p2) messages. */
    val ackState: AckState,
    /** Receipt id for p2 messages; null for non-emergency messages. */
    val receiptId: String?,
)
