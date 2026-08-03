package net.pushfree.android.ui

import android.content.Context
import androidx.room.Room
import net.pushfree.android.data.PushFreeDatabase
import net.pushfree.android.ui.onboarding.OkHttpDeviceRegistrar
import net.pushfree.android.ui.settings.NotificationTester
import net.pushfree.android.ui.settings.PushfreeNotificationTester
import net.pushfree.android.ui.settings.SharedPrefsTransportPreferenceStore
import okhttp3.OkHttpClient

/**
 * Process-wide dependencies constructed once from the application context and
 * passed to the Compose ViewModels.
 */
class AppContainer(context: Context) {
    val database: PushFreeDatabase = Room.databaseBuilder(
        context.applicationContext,
        PushFreeDatabase::class.java,
        PushFreeDatabase.NAME,
    ).fallbackToDestructiveMigration(false).build()

    val repository: SubscriptionRepository =
        RoomSubscriptionRepository(database.subscriptionDao(), database.messageDao())

    val httpClient: OkHttpClient = OkHttpDeviceRegistrar.defaultClient()

    val registrar = OkHttpDeviceRegistrar(
        client = httpClient,
        os = "android",
        model = android.os.Build.MODEL ?: "",
    )

    val transportStore = SharedPrefsTransportPreferenceStore(context.applicationContext)

    val tester: NotificationTester =
        PushfreeNotificationTester(context.applicationContext)
}
