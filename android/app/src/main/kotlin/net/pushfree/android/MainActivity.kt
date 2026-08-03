package net.pushfree.android

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import net.pushfree.android.notifications.Notifications
import net.pushfree.android.ui.AppContainer
import net.pushfree.android.ui.PushfreeApp
import net.pushfree.android.ui.theme.PushfreeTheme

/**
 * Single launch activity. Hosts the Compose UI (subscription list, message
 * detail, add-server onboarding, settings) wired through [PushfreeApp].
 */
class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Channels must exist before any notification is posted; create them at
        // process start so FCM/UnifiedPush/WorkManager posters can rely on them.
        Notifications.ensureChannels(this)
        enableEdgeToEdge()
        val container = AppContainer(this)
        setContent {
            PushfreeTheme {
                PushfreeApp(container = container)
            }
        }
    }
}
