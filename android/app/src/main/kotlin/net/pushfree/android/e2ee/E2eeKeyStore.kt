package net.pushfree.android.e2ee

import android.content.Context

/**
 * Persists the user's E2EE key (64-char hex, todo 44) so it survives process
 * restart. Stored out-of-band — the server never receives it. Mirrors the
 * SharedPreferences pattern in [net.pushfree.android.ui.settings.SharedPrefsTransportPreferenceStore].
 *
 * An interface so transport ingest hooks are unit-testable with an in-memory
 * fake; production wires [SharedPrefsE2eeKeyStore].
 */
interface E2eeKeyStore {
    /** The configured key, or null when unset/blank. */
    fun get(): String?

    /** Set (or clear, with null/blank) the E2EE key. */
    fun set(value: String?)
}

/** SharedPreferences-backed [E2eeKeyStore]. */
class SharedPrefsE2eeKeyStore(context: Context) : E2eeKeyStore {
    private val prefs =
        context.applicationContext.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    override fun get(): String? =
        prefs.getString(KEY, null)?.takeIf { it.isNotEmpty() }

    override fun set(value: String?) {
        val editor = prefs.edit()
        if (value.isNullOrEmpty()) {
            editor.remove(KEY)
        } else {
            editor.putString(KEY, value)
        }
        editor.apply()
    }

    private companion object {
        const val PREFS = "pushfree_e2ee"
        const val KEY = "e2ee_key"
    }
}
