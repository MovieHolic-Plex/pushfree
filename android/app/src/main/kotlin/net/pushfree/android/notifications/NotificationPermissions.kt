package net.pushfree.android.notifications

import android.Manifest
import android.app.Activity
import android.app.NotificationManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.provider.Settings
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat

/**
 * Runtime permission helpers for the notification pipeline.
 *
 * Building a notification never depends on the POST_NOTIFICATIONS grant state
 * — notifications are always constructed; this helper only governs whether the
 * system will *display* them and offers the API 33+ request flow.
 */
object NotificationPermission {
    const val REQUEST_POST_NOTIFICATIONS = 4201

    /** True when POST_NOTIFICATIONS is granted (always true below API 33). */
    fun isGranted(context: Context): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return true
        return ContextCompat.checkSelfPermission(
            context, Manifest.permission.POST_NOTIFICATIONS,
        ) == PackageManager.PERMISSION_GRANTED
    }

    /**
     * Request POST_NOTIFICATIONS (API 33+). No-op below API 33 where the
     * permission is implicit at install time.
     */
    fun request(activity: Activity) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return
        ActivityCompat.requestPermissions(
            activity,
            arrayOf(Manifest.permission.POST_NOTIFICATIONS),
            REQUEST_POST_NOTIFICATIONS,
        )
    }
}

/**
 * Android 14+ (API 34) USE_FULL_SCREEN_INTENT permission flow (H5).
 *
 * Below API 34 full-screen intents are usable unconditionally, so [isGranted]
 * returns true. On API 34+ it reflects the actual grant via
 * [NotificationManager.canUseFullScreenIntent]; when false the caller should
 * surface [settingsIntent] and the emergency notification must fall back to a
 * heads-up + strong vibration (handled in [PushfreeNotificationBuilder]).
 */
object FullScreenIntentPermission {
    /** True below API 34; on API 34+ reflects the system grant. */
    fun isGranted(context: Context): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.UPSIDE_DOWN_CAKE) return true
        val nm = context.getSystemService(NotificationManager::class.java) ?: return false
        return nm.canUseFullScreenIntent()
    }

    /** Intent that opens the system "allow full-screen notifications" settings. */
    fun settingsIntent(context: Context): Intent =
        Intent(Settings.ACTION_MANAGE_APP_USE_FULL_SCREEN_INTENT).apply {
            data = Uri.parse("package:${context.packageName}")
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        }
}
