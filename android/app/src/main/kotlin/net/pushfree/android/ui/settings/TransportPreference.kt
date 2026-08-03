package net.pushfree.android.ui.settings

import android.content.Context
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.onStart

/**
 * Available transports the client can receive messages over. The value is
 * persisted so the user's choice survives process restart; the WS foreground
 * service and FCM/UnifiedPush sources read it at startup.
 */
enum class TransportPreference(val label: String) {
    WEBSOCKET("WebSocket"),
    FCM("FCM"),
}

/**
 * Persisted transport selector. Kept as an interface so [SettingsViewModel]
 * tests use a deterministic in-memory fake; production wires
 * [SharedPrefsTransportPreferenceStore] to the app's default SharedPreferences.
 */
interface TransportPreferenceStore {
    fun observe(): Flow<TransportPreference>
    fun set(value: TransportPreference)
}

/** SharedPreferences-backed implementation. */
class SharedPrefsTransportPreferenceStore(context: Context) : TransportPreferenceStore {
    private val prefs = context.applicationContext.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    override fun observe(): Flow<TransportPreference> = callbackFlow {
        val listener = android.content.SharedPreferences.OnSharedPreferenceChangeListener { _, key ->
            if (key == KEY_TRANSPORT) trySend(current())
        }
        prefs.registerOnSharedPreferenceChangeListener(listener)
        trySend(current())
        awaitClose { prefs.unregisterOnSharedPreferenceChangeListener(listener) }
    }
        .onStart { emit(current()) }
        .distinctUntilChanged()

    override fun set(value: TransportPreference) {
        prefs.edit().putString(KEY_TRANSPORT, value.name).apply()
    }

    private fun current(): TransportPreference =
        runCatching {
            TransportPreference.valueOf(prefs.getString(KEY_TRANSPORT, TransportPreference.WEBSOCKET.name)!!)
        }.getOrDefault(TransportPreference.WEBSOCKET)

    private companion object {
        const val PREFS = "pushfree_ui_prefs"
        const val KEY_TRANSPORT = "transport"
    }
}
