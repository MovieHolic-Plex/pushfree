package net.pushfree.android.data

import androidx.room.TypeConverter

/** Acknowledgement lifecycle state for an emergency-priority (p2) message receipt. */
enum class AckState {
    /** Not an emergency message; no receipt involved. */
    NONE,

    /** Receipt created, awaiting acknowledgement. */
    PENDING,

    /** Acknowledged by the user (or delivery-confirmed + ack). */
    ACKED,
}

/** Room type converters for value types stored in entities. */
class Converters {
    @TypeConverter
    fun fromAckState(state: AckState): String = state.name

    @TypeConverter
    fun toAckState(value: String): AckState = AckState.valueOf(value)
}
