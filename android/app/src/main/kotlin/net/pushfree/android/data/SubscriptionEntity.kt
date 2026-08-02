package net.pushfree.android.data

import androidx.room.Entity
import androidx.room.PrimaryKey

/**
 * One row per configured server subscription. The server URL is the natural key:
 * the client tracks at most one subscription per server it talks to, so upserting
 * the same [serverUrl] replaces the stored credentials in place.
 */
@Entity(tableName = "subscriptions")
data class SubscriptionEntity(
    /** Base URL of the pushfree server this subscription belongs to. */
    @PrimaryKey val serverUrl: String,
    /** Recipient user_key on this server (Pushover-compatible 30-char identifier). */
    val userKey: String,
    /** App token used to authenticate sends / quota for this subscription. */
    val token: String,
    /** Registered device id (Open Client login) used for WS/SSE auth. */
    val deviceId: String,
    /** Device secret paired with [deviceId]; sent to fetch messages / open WS. */
    val secret: String,
)
