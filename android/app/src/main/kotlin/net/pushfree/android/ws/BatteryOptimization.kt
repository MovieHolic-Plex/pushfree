package net.pushfree.android.ws

import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings

/**
 * Battery-optimization exemption flow for the WS transport.
 *
 * Doze can defer/limit a foreground service's work on some OEMs; placing the app
 * on the battery-optimization allowlist is the reliable way to keep the
 * persistent WebSocket live. This helper reports the allowlist state and launches
 * the system prompt (`ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`) for the UI
 * to surface during onboarding.
 */
object BatteryOptimization {
    /** True when this app is already exempt from battery optimization. */
    fun isExempt(context: Context): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) return true
        val pm = context.getSystemService(PowerManager::class.java) ?: return false
        return pm.isIgnoringBatteryOptimizations(context.packageName)
    }

    /**
     * Launches the system "ignore battery optimizations" prompt for this app.
     * No-op (returns true) below API 23 where the concept does not exist.
     * Returns false if the intent could not be started.
     */
    fun requestExemption(context: Context): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) return true
        return runCatching {
            val intent = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS).apply {
                data = Uri.parse("package:" + context.packageName)
                addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            }
            context.startActivity(intent)
            true
        }.getOrDefault(false)
    }
}
