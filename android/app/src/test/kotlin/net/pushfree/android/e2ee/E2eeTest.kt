package net.pushfree.android.e2ee

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/**
 * THE cross-platform E2EE vector test (plan todo 44).
 *
 * Consumes the SAME shared fixture committed at
 * `server/internal/e2ee/testdata/e2ee_vectors.json` that the Go suite
 * (`server/internal/e2ee/probe_test.go::TestSharedVectors`) and the desktop
 * suite (`desktop/src/e2ee.rs::shared_vectors_match_go_and_android`) consume.
 * Positive cases must decrypt to the exact plaintext; negative cases (wrong
 * key, tampered HMAC) must fail. Identical vectors pass on all three platforms.
 *
 * Pure-JVM unit test (JDK crypto + org.json on the local classpath — no
 * Robolectric, no network, no Android types).
 */
class E2eeTest {

    // ---- shared fixture consumption ---------------------------------------

    /**
     * Locate the shared fixture by walking up from the test working directory
     * looking for `server/internal/e2ee/testdata/e2ee_vectors.json`. Robust to
     * the module the test runs from (AGP unit tests run with the module dir as
     * the working directory).
     */
    private fun fixtureFile(): File {
        var dir: File? = File(".").canonicalFile
        while (dir != null) {
            val candidate = File(dir, "server/internal/e2ee/testdata/e2ee_vectors.json")
            if (candidate.exists()) return candidate
            // Walk up to the repo root regardless of the module the test runs
            // from (AGP unit tests run with the module dir as working dir).
            dir = dir.parentFile
        }
        // Fallback relative path.
        return File("../../server/internal/e2ee/testdata/e2ee_vectors.json")
    }

    private fun vectors(): List<VectorCase> {
        val text = fixtureFile().readText(Charsets.UTF_8)
        val json = JSONObject(text)
        val arr = json.getJSONArray("vectors")
        val out = ArrayList<VectorCase>(arr.length())
        for (i in 0 until arr.length()) {
            val o = arr.getJSONObject(i)
            out += VectorCase(
                name = o.getString("name"),
                keyHex = o.getString("key_hex"),
                plaintext = o.optString("plaintext"),
                blob = o.getString("blob"),
                expectError = o.optBoolean("expect_error", false),
            )
        }
        assertTrue("fixture must contain vectors", out.isNotEmpty())
        return out
    }

    private data class VectorCase(
        val name: String,
        val keyHex: String,
        val plaintext: String,
        val blob: String,
        val expectError: Boolean,
    )

    @Test
    fun shared_vectors_match_go_and_desktop() {
        val cases = vectors()
        for (c in cases) {
            val result = E2ee.decryptField(c.keyHex, c.blob)
            if (c.expectError) {
                assertNull(
                    "${c.name}: expected decryption failure, got $result",
                    result,
                )
            } else {
                assertEquals(
                    "${c.name}: plaintext mismatch",
                    c.plaintext,
                    result,
                )
            }
        }
        // Raw output for evidence / misleading-success check: print the fixture
        // path plus every decrypted plaintext so a human can eyeball real bytes.
        println("[e2ee/vector-file] ${fixtureFile().absolutePath}")
        for (c in cases) {
            if (c.expectError) {
                println("[e2ee/decrypt] ${c.name} -> (error, as expected)")
            } else {
                println("[e2ee/decrypt] ${c.name} -> ${c.plaintext}")
            }
        }
    }

    // ---- key parsing ------------------------------------------------------

    @Test
    fun parse_key_rejects_wrong_length_and_non_hex() {
        assertNull(E2ee.parseKey(""))
        assertNull(E2ee.parseKey("toolong"))
        assertNull(E2ee.parseKey("z".repeat(64))) // right length, non-hex
        assertNull(E2ee.parseKey("a".repeat(63))) // one short
        val key = E2ee.parseKey("0".repeat(64))
        assertEquals(E2ee.KEY_LEN, key!!.size)
    }

    @Test
    fun parse_key_matches_decode_of_canonical_vector() {
        val canonical = vectors().first { it.name == "canonical" }
        val key = E2ee.parseKey(canonical.keyHex)!!
        // First byte 0x00, last byte 0x1f for the canonical 00..1f key.
        assertEquals(0x00, key[0].toInt() and 0xFF)
        assertEquals(0x1f, key[31].toInt() and 0xFF)
    }

    // ---- failure policy: never crash, never leak --------------------------

    @Test
    fun decrypt_wrong_key_size_returns_null() {
        val canonical = vectors().first { it.name == "canonical" }
        assertNull(E2ee.decrypt(ByteArray(7), canonical.blob))
    }

    @Test
    fun decrypt_bad_base64_returns_null() {
        assertNull(E2ee.decrypt(ByteArray(E2ee.KEY_LEN), "!!!!not-base64!!!!"))
    }

    @Test
    fun decrypt_short_blob_returns_null() {
        assertNull(E2ee.decrypt(ByteArray(E2ee.KEY_LEN), "AAAAAAAA"))
    }

    // ---- message-level decryptTitleBody hook (WS/FCM/UP ingest) -----------

    @Test
    fun decrypt_title_body_plaintext_passes_through_unchanged() {
        // Not encrypted -> returned verbatim; a missing key never degrades it.
        val (title, body) = E2ee.decryptTitleBody(
            title = "plain-title",
            body = "plain-body",
            encrypted = false,
            hexKey = null,
        )
        assertEquals("plain-title", title)
        assertEquals("plain-body", body)
    }

    @Test
    fun decrypt_title_body_correct_key_shows_plaintext() {
        val cases = vectors()
        val canonical = cases.first { it.name == "canonical" }
        val short = cases.first { it.name == "short" }
        val (title, body) = E2ee.decryptTitleBody(
            title = canonical.blob,
            body = short.blob,
            encrypted = true,
            hexKey = canonical.keyHex,
        )
        assertEquals("Pushfree E2EE test vector", title)
        assertEquals("hi", body)
    }

    @Test
    fun decrypt_title_body_wrong_key_shows_placeholder_never_ciphertext() {
        val canonical = vectors().first { it.name == "canonical" }
        val wrongKey = "f".repeat(64)
        val (title, body) = E2ee.decryptTitleBody(
            title = canonical.blob,
            body = canonical.blob,
            encrypted = true,
            hexKey = wrongKey,
        )
        assertEquals(E2ee.DECRYPT_FAILED_PLACEHOLDER, title)
        assertEquals(E2ee.DECRYPT_FAILED_PLACEHOLDER, body)
        // CRITICAL: the ciphertext blob must NEVER reach the UI on failure.
        assertNotEquals(canonical.blob, title)
        assertNotEquals(canonical.blob, body)
    }

    @Test
    fun decrypt_title_body_missing_key_shows_placeholder() {
        val canonical = vectors().first { it.name == "canonical" }
        val (title, body) = E2ee.decryptTitleBody(
            title = canonical.blob,
            body = canonical.blob,
            encrypted = true,
            hexKey = null,
        )
        assertEquals(E2ee.DECRYPT_FAILED_PLACEHOLDER, title)
        assertEquals(E2ee.DECRYPT_FAILED_PLACEHOLDER, body)
    }

    @Test
    fun decrypt_title_body_empty_fields_stay_empty() {
        val (title, body) = E2ee.decryptTitleBody(
            title = null,
            body = "",
            encrypted = true,
            hexKey = "0".repeat(64),
        )
        assertNull(title)
        assertEquals("", body)
    }

    @Test
    fun decrypt_display_returns_placeholder_for_empty_key() {
        assertEquals("", E2ee.decryptDisplay("", "0".repeat(64)))
        assertEquals(E2ee.DECRYPT_FAILED_PLACEHOLDER, E2ee.decryptDisplay("ABC", null))
        assertEquals(E2ee.DECRYPT_FAILED_PLACEHOLDER, E2ee.decryptDisplay("ABC", ""))
    }

    @Test
    fun placeholder_is_never_ciphertext_for_any_vector() {
        // For every positive vector, the blob itself is NOT the placeholder and
        // a failed decrypt of it yields the placeholder (defense against ever
        // showing raw ciphertext).
        for (c in vectors()) {
            if (c.expectError) continue
            assertNotEquals(E2ee.DECRYPT_FAILED_PLACEHOLDER, c.blob)
            val wrongKey = "f".repeat(64)
            val failed = E2ee.decryptDisplay(c.blob, wrongKey)
            assertEquals(
                "wrong key of ${c.name} must yield placeholder",
                E2ee.DECRYPT_FAILED_PLACEHOLDER,
                failed,
            )
            assertFalse(
                "${c.name}: failed decrypt must not equal the blob",
                failed == c.blob,
            )
        }
    }
}
