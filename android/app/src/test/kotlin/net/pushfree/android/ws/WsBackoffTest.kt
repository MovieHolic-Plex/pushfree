package net.pushfree.android.ws

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import kotlin.random.Random
import kotlin.time.Duration

/**
 * Pure unit tests for the Full-Jitter backoff calculator. No clock and no
 * randomness from the environment — the cap sequence is deterministic and the
 * jitter bounds are asserted with a seeded [Random].
 *
 * Acceptance: the cap sequence is `[1,2,4,...,<=60]` seconds and every drawn
 * delay lies within `[0, cap]`.
 */
class WsBackoffTest {

    // 1. The advertised cap sequence: 1,2,4,8,16,32, then clamped to 60.
    @Test
    fun cap_sequence_is_one_two_four_capped_at_sixty() {
        val caps = (0..8).map { WsBackoff.cap(it).inWholeSeconds }
        assertEquals(listOf(1L, 2L, 4L, 8L, 16L, 32L, 60L, 60L, 60L), caps)
    }

    // 2. The cap never escapes [0, 60s] for a wide range of attempts (no overflow).
    @Test
    fun cap_never_negative_or_above_sixty_seconds() {
        for (attempt in 0..63) {
            val cap = WsBackoff.cap(attempt)
            assertTrue("attempt $attempt cap $cap < 0", cap >= Duration.ZERO)
            assertTrue("attempt $attempt cap $cap > CAP", cap <= WsBackoff.CAP)
        }
    }

    // 3. Full-jitter draws always lie within [0, cap(attempt)].
    @Test
    fun delay_is_within_zero_to_cap_bounds() {
        val rng = Random(0xC0FFEE)
        for (attempt in 0..30) {
            val cap = WsBackoff.cap(attempt)
            repeat(SAMPLES) {
                val d = WsBackoff.delay(attempt, rng)
                assertTrue("attempt $attempt drew $d < 0", d >= Duration.ZERO)
                assertTrue("attempt $attempt drew $d > cap $cap", d <= cap)
            }
        }
    }

    // 4. Jitter is real: draws vary and include values well below the cap.
    @Test
    fun delay_jitters_below_cap() {
        val samples = (0..SAMPLES).map { WsBackoff.delay(5, Random(it.toLong())) }
        assertTrue(
            "full jitter must produce draws below the cap",
            samples.any { it < WsBackoff.cap(5) },
        )
        assertTrue(
            "full jitter must produce small draws (< half cap)",
            samples.any { it.inWholeSeconds < WsBackoff.cap(5).inWholeSeconds / 2 },
        )
    }

    // 5. Determinism: a seeded Random reproduces the exact same delay.
    @Test
    fun delay_is_deterministic_with_seeded_random() {
        val attempt = 4
        assertEquals(
            WsBackoff.delay(attempt, Random(7)),
            WsBackoff.delay(attempt, Random(7)),
        )
    }

    private companion object {
        const val SAMPLES = 500
    }
}
