package net.pushfree.android.ui

import android.content.Intent
import android.net.Uri
import android.provider.Settings
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.platform.LocalContext
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.lifecycle.viewmodel.initializer
import androidx.lifecycle.viewmodel.viewModelFactory
import net.pushfree.android.notifications.FullScreenIntentPermission
import net.pushfree.android.notifications.NotificationPermission
import net.pushfree.android.ui.detail.MessageDetailScreen
import net.pushfree.android.ui.detail.MessageDetailViewModel
import net.pushfree.android.ui.onboarding.AddServerScreen
import net.pushfree.android.ui.onboarding.AddServerViewModel
import net.pushfree.android.ui.settings.SettingsScreen
import net.pushfree.android.ui.settings.SettingsViewModel
import net.pushfree.android.ui.subscription.SubscriptionListScreen
import net.pushfree.android.ui.subscription.SubscriptionListViewModel
import net.pushfree.android.ws.BatteryOptimization

/** In-app navigation routes (no extra navigation dependency). */
sealed interface Route {
    data object List : Route
    data object AddServer : Route
    data object Settings : Route
    data class Detail(val messageId: Long) : Route
}

/**
 * Top-level host: holds the nav state and wires each screen's ViewModel from
 * [AppContainer]. Permission/system flags are re-read whenever they may have
 * changed and pushed into the settings ViewModel.
 */
@Composable
fun PushfreeApp(container: AppContainer) {
    var route by rememberSaveable(stateSaver = RouteSaver) { mutableStateOf<Route>(Route.List) }
    val context = LocalContext.current

    when (val current = route) {
        Route.List -> {
            val vm: SubscriptionListViewModel = viewModel(factory = listFactory(container))
            SubscriptionListScreen(
                viewModel = vm,
                onMessageClick = { id -> route = Route.Detail(id) },
                onAddServerClick = { route = Route.AddServer },
                onSettingsClick = { route = Route.Settings },
            )
        }

        Route.AddServer -> {
            val vm: AddServerViewModel = viewModel(factory = addServerFactory(container))
            AddServerScreen(
                viewModel = vm,
                onBack = { route = Route.List },
                onRegistered = {
                    vm.reset()
                    route = Route.List
                },
            )
        }

        Route.Settings -> {
            val vm: SettingsViewModel = viewModel(factory = settingsFactory(container))
            val settingsReturn = rememberLauncherForActivityResult(
                ActivityResultContracts.StartActivityForResult(),
            ) { refreshPermissions(vm, context) }

            LaunchedEffect(Unit) { refreshPermissions(vm, context) }

            SettingsScreen(
                viewModel = vm,
                onBack = { route = Route.List },
                onOpenNotificationSettings = {
                    runCatching {
                        settingsReturn.launch(
                            Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS).apply {
                                putExtra(Settings.EXTRA_APP_PACKAGE, context.packageName)
                            },
                        )
                    }
                },
                onOpenBatterySettings = {
                    val ok = BatteryOptimization.requestExemption(context)
                    if (!ok) launchAppDetailsSettings(context)
                    refreshPermissions(vm, context)
                },
                onOpenFullScreenIntentSettings = {
                    runCatching { context.startActivity(FullScreenIntentPermission.settingsIntent(context)) }
                    refreshPermissions(vm, context)
                },
            )
        }

        is Route.Detail -> {
            val vm: MessageDetailViewModel = viewModel(
                key = "detail-${current.messageId}",
                factory = detailFactory(container, current.messageId),
            )
            MessageDetailScreen(viewModel = vm, onBack = { route = Route.List })
        }
    }
}

private fun refreshPermissions(vm: SettingsViewModel, context: android.content.Context) {
    vm.setPermissionFlags(
        postGranted = NotificationPermission.isGranted(context),
        batteryExempt = BatteryOptimization.isExempt(context),
        fsiGranted = FullScreenIntentPermission.isGranted(context),
    )
}

private fun launchAppDetailsSettings(context: android.content.Context) {
    runCatching {
        val intent = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS).apply {
            data = Uri.fromParts("package", context.packageName, null)
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        }
        context.startActivity(intent)
    }
}

private fun listFactory(container: AppContainer) = viewModelFactory {
    initializer { SubscriptionListViewModel(container.repository) }
}

private fun addServerFactory(container: AppContainer) = viewModelFactory {
    AddServerViewModel(container.registrar, container.repository)
}

private fun settingsFactory(container: AppContainer) = viewModelFactory {
    SettingsViewModel(container.transportStore, container.tester)
}

private fun detailFactory(container: AppContainer, messageId: Long) = viewModelFactory {
    MessageDetailViewModel(container.repository, messageId)
}
