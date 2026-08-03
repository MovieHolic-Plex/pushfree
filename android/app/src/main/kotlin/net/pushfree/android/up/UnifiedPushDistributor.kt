package net.pushfree.android.up

import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.util.Log

/**
 * On-device UnifiedPush distributor discovery and the connector->distributor
 * register/unregister broadcast trigger.
 *
 * Fallback (spec): if no distributor app is installed, the UP transport is
 * gracefully disabled and the WebSocket foreground service stays the primary
 * source — [isAvailable] returns false and [register] is a logged no-op, so the
 * app never crashes or blocks onboarding when UP is absent.
 */
object UnifiedPushDistributor {

    /**
     * True iff at least one installed app advertises a receiver for the
     * connector REGISTER action (i.e. a distributor is present). Drives the
     * "no distributor -> disabled, WS primary" fallback.
     */
    fun isAvailable(context: Context): Boolean = matchingReceivers(context).isNotEmpty()

    /**
     * Ask the distributor to register this app instance. No-op (logged) when no
     * distributor is installed, so callers can invoke it unconditionally and
     * the transport simply stays disabled.
     */
    fun register(context: Context, instance: String = UnifiedPushContract.DEFAULT_INSTANCE) {
        if (!isAvailable(context)) {
            Log.i(TAG, "UP register skipped: no distributor installed (WS stays primary)")
            return
        }
        context.sendBroadcast(
            Intent(UnifiedPushContract.ACTION_REGISTER).apply {
                putExtra(UnifiedPushContract.EXTRA_APPLICATION, context.packageName)
                putExtra(UnifiedPushContract.EXTRA_INSTANCE, instance)
            },
        )
    }

    /** Ask the distributor to unregister this app instance. */
    fun unregister(context: Context, instance: String = UnifiedPushContract.DEFAULT_INSTANCE) {
        context.sendBroadcast(
            Intent(UnifiedPushContract.ACTION_UNREGISTER).apply {
                putExtra(UnifiedPushContract.EXTRA_APPLICATION, context.packageName)
                putExtra(UnifiedPushContract.EXTRA_INSTANCE, instance)
            },
        )
    }

    private fun matchingReceivers(context: Context) =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            context.packageManager.queryBroadcastReceivers(
                Intent(UnifiedPushContract.ACTION_REGISTER),
                PackageManager.ResolveInfoFlags.of(0),
            )
        } else {
            @Suppress("DEPRECATION")
            context.packageManager.queryBroadcastReceivers(
                Intent(UnifiedPushContract.ACTION_REGISTER),
                0,
            )
        }

    private const val TAG = "UnifiedPushDistributor"
}
