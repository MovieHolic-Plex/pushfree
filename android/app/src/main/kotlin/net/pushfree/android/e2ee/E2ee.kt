package net.pushfree.android.e2ee

import java.io.ByteArrayInputStream
import java.security.MessageDigest
import java.util.Base64
import java.util.zip.GZIPInputStream
import javax.crypto.Cipher
import javax.crypto.Mac
import javax.crypto.spec.IvParameterSpec
import javax.crypto.spec.SecretKeySpec

/**
 * End-to-end-encryption field decryption (plan todo 44).
 *
 * Reverses the server's opaque field format defined in
 * `server/internal/e2ee/e2ee.go` (todo 43 — the wire-format source of truth):
 *
 * ```
 * gzip(plaintext)
 *   -> AES-256-CBC (random 16-byte IV, PKCS7 padding)
 *   -> HMAC-SHA256(key, IV || ciphertext)
 *   -> base64( IV || ciphertext || hmac )
 * ```
 *
 * The 256-bit key is a 64-character hex string provisioned out-of-band; the
 * server never receives it. Each of message/title/url/url_title is encrypted
 * independently with its own fresh IV + HMAC, so each field is decrypted
 * independently here.
 *
 * # Failure policy (research EB/W2-E2EE; plan todo 44)
 * A wrong key, a tampered HMAC, bad padding, or a corrupt gzip stream MUST
 * surface as a safe placeholder — never an exception, never ciphertext, never
 * garbage plaintext. Every public decrypt function returns `null` on ANY
 * failure (the caller decides the placeholder), mirroring the Go reference's
 * padding-oracle hygiene: the MAC is verified BEFORE any CBC / PKCS7 work
 * (encrypt-then-MAC), and no detail distinguishes a MAC failure from a padding
 * failure. Uses only JDK crypto so it runs as a fast plain-JVM unit test.
 */
object E2ee {

    const val KEY_LEN: Int = 32
    const val KEY_HEX_LEN: Int = 64
    const val IV_LEN: Int = 16
    const val HMAC_LEN: Int = 32

    /** AES block size in bytes. */
    private const val BLOCK_LEN: Int = 16

    /** Smallest legal raw blob: IV + exactly one AES block + HMAC. */
    private const val MIN_BLOB_LEN: Int = IV_LEN + BLOCK_LEN + HMAC_LEN

    /**
     * Placeholder shown for an encrypted field that cannot be decrypted. Never
     * the ciphertext, never partially-decrypted garbage.
     */
    const val DECRYPT_FAILED_PLACEHOLDER: String = "[undecryptable]"

    /**
     * Decode the 64-character hex form of an E2EE key into 32 raw bytes.
     * Returns `null` for any wrong length or non-hex character (never throws).
     */
    fun parseKey(hexKey: String): ByteArray? {
        if (hexKey.length != KEY_HEX_LEN) return null
        val out = ByteArray(KEY_LEN)
        var i = 0
        var j = 0
        while (i < KEY_HEX_LEN) {
            val hi = hexNibble(hexKey[i]) ?: return null
            val lo = hexNibble(hexKey[i + 1]) ?: return null
            out[j++] = ((hi shl 4) or lo).toByte()
            i += 2
        }
        return out
    }

    /**
     * Decrypt a single base64 field [blob] under a 32-byte [key]. Returns the
     * plaintext bytes, or `null` on ANY failure (bad base64, short blob, MAC
     * mismatch, bad padding, corrupt gzip). Never throws.
     */
    fun decrypt(key: ByteArray, blob: String): ByteArray? {
        if (key.size != KEY_LEN) return null
        val raw = try {
            Base64.getDecoder().decode(blob)
        } catch (_: IllegalArgumentException) {
            return null
        }
        if (raw.size < MIN_BLOB_LEN) return null
        val ctLen = raw.size - IV_LEN - HMAC_LEN
        if (ctLen <= 0 || ctLen % BLOCK_LEN != 0) return null
        val macOff = raw.size - HMAC_LEN

        // Verify-then-decrypt. MessageDigest.isEqual is constant-time.
        val want = hmacSha256(key, raw, 0, macOff)
        val mac = raw.copyOfRange(macOff, raw.size)
        if (!MessageDigest.isEqual(want, mac)) return null

        // CBC decrypt (NoPadding: we validate PKCS7 ourselves).
        val iv = raw.copyOfRange(0, IV_LEN)
        val ct = raw.copyOfRange(IV_LEN, macOff)
        val padded = try {
            val cipher = Cipher.getInstance("AES/CBC/NoPadding")
            cipher.init(Cipher.DECRYPT_MODE, SecretKeySpec(key, "AES"), IvParameterSpec(iv))
            cipher.doFinal(ct)
        } catch (_: Exception) {
            return null
        }

        // PKCS7 unpad with full validation. Any anomaly -> null.
        val padLen = padded.last().toInt() and 0xFF
        if (padLen == 0 || padLen > BLOCK_LEN) return null
        val padStart = padded.size - padLen
        for (k in padStart until padded.size) {
            if ((padded[k].toInt() and 0xFF) != padLen) return null
        }
        val compressed = padded.copyOfRange(0, padStart)

        // gunzip
        return try {
            GZIPInputStream(ByteArrayInputStream(compressed)).use { it.readBytes() }
        } catch (_: Exception) {
            null
        }
    }

    /**
     * String-in/string-out convenience. Returns the plaintext, or `null` on any
     * failure or if the decrypted bytes are not valid UTF-8. Never throws.
     */
    fun decryptField(hexKey: String, blob: String): String? {
        val key = parseKey(hexKey) ?: return null
        val pt = decrypt(key, blob) ?: return null
        return try {
            String(pt, Charsets.UTF_8)
        } catch (_: Exception) {
            null
        }
    }

    /**
     * Decrypt a single display field applying the message-level failure policy.
     * - empty [blob] -> "" (the field carried no ciphertext)
     * - null/empty [hexKey] -> [DECRYPT_FAILED_PLACEHOLDER] (no key configured)
     * - decryption failure -> [DECRYPT_FAILED_PLACEHOLDER] (wrong key / tampered)
     * Never returns the ciphertext. Never throws.
     */
    fun decryptDisplay(blob: String, hexKey: String?): String {
        if (blob.isEmpty()) return ""
        if (hexKey.isNullOrEmpty()) return DECRYPT_FAILED_PLACEHOLDER
        return decryptField(hexKey, blob) ?: DECRYPT_FAILED_PLACEHOLDER
    }

    /**
     * Decrypt the title/body of an encrypted message for storage/display,
     * applying the failure policy at message granularity. Used by every
     * transport's ingest hook (WS/FCM/UP -> MessageEntity, todo 28/32).
     *
     * When [encrypted] is false the fields are returned unchanged (the common
     * plaintext path, so a missing key never degrades a plaintext message).
     * When true, a null/empty [hexKey] or any decryption failure yields the
     * placeholder for each non-empty field — an error state, never ciphertext.
     */
    fun decryptTitleBody(
        title: String?,
        body: String,
        encrypted: Boolean,
        hexKey: String?,
    ): Pair<String?, String> {
        if (!encrypted) return title to body
        val dec = { b: String -> decryptDisplay(b, hexKey) }
        val decTitle = if (title.isNullOrEmpty()) title else dec(title)
        return decTitle to dec(body)
    }

    private fun hmacSha256(key: ByteArray, data: ByteArray, offset: Int, len: Int): ByteArray {
        val mac = Mac.getInstance("HmacSHA256")
        mac.init(SecretKeySpec(key, "HmacSHA256"))
        mac.update(data, offset, len)
        return mac.doFinal()
    }

    private fun hexNibble(c: Char): Int? = when (c) {
        in '0'..'9' -> c - '0'
        in 'a'..'f' -> c - 'a' + 10
        in 'A'..'F' -> c - 'A' + 10
        else -> null
    }
}
