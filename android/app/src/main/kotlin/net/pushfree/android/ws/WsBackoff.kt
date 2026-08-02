package net.pushfree.android.ws

import kotlin.math.min
import kotlin.random.Random
import kotlin.time.Duration
import kotlin.time.Duration.Companion.milliseconds
import kotlin.time.Duration.Companion.seconds

/**
 * Full-Jitter exponential backoff for the WS reconnect loop (AWS "Exponential
 * Backoff With Jitter", full-jitter mode), per the pushfree client transport
 * contract: base 1s, cap 60s.
 *
 * Full jitter draws the sleep uniformly from `[0, cap(attempt)]`, where
 * `cap(attempt) = min(CAP, BASE * 2^attempt)`. Capping the *maximum* before
 * jitter — rather than adding jitter to a fixed value — bounds the worst-case
 * delay and thunders the reconnect herd least (each client independently picks
 * a random point in `[0, cap]`).
 *
 * The cap sequence is therefore `[1, 2, 4, 8, 16, 32, 60, 60, ...]` seconds
 * (64s at attempt 6 clamps to the 60s cap), and every realised delay lies in
 * `[0, cap]`. `cap` is pure and `delay` takes an injectable [Random], so both
 * the sequence and the jitter bounds are asserted in tests with no wall clock.
 */
object WsBackoff {
    /** Minimum / first-attempt delay before jitter. */
    val BASE: Duration = 1.seconds

    /** Hard ceiling on any single reconnect delay. */
    val CAP: Duration = 60.seconds

    /**
     * Maximum delay (pre-jitter) for a 0-indexed [attempt]. Doubles from [BASE]
     * each step until it reaches [CAP], then stays there.
     */
    fun cap(attempt: Int): Duration {
        require(attempt >= 0) { "attempt must be non-negative: $attempt" }
        // BASE * 2^attempt overflows Long at extreme attempt; CAP is reached at
        // attempt 6 (1s * 2^6 = 64s > 60s), so clamp there and stay stable.
        if (attempt >= CAP_ATTEMPT) return CAP
        val rawMs = BASE.inWholeMilliseconds shl attempt
        return min(rawMs, CAP.inWholeMilliseconds).milliseconds
    }

    /**
     * Full-jitter delay for [attempt] drawn with [random]: a uniform value in
     * `[0, cap(attempt)]` inclusive of both bounds.
     */
    fun delay(attempt: Int, random: Random): Duration {
        val boundMs = cap(attempt).inWholeMilliseconds
        if (boundMs <= 0L) return Duration.ZERO
        // nextLong(0, boundMs + 1) -> [0, boundMs] inclusive.
        return random.nextLong(0, boundMs + 1).milliseconds
    }

    /** Attempt index at/after which the cap is saturated (1s * 2^6 = 64s >= 60s). */
    private const val CAP_ATTEMPT = 6
}
