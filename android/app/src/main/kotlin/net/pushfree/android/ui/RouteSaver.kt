package net.pushfree.android.ui

import androidx.compose.runtime.saveable.Saver

/** Saves the [Route] nav state across process death/configuration change. */
val RouteSaver: Saver<Route, String> = Saver(
    save = { route ->
        when (route) {
            Route.List -> "list"
            Route.AddServer -> "add"
            Route.Settings -> "settings"
            is Route.Detail -> "detail:${route.messageId}"
        }
    },
    restore = { saved ->
        when {
            saved == "list" -> Route.List
            saved == "add" -> Route.AddServer
            saved == "settings" -> Route.Settings
            saved.startsWith("detail:") ->
                saved.removePrefix("detail:").toLongOrNull()?.let { Route.Detail(it) } ?: Route.List
            else -> Route.List
        }
    },
)
