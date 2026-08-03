// Top-level build file. Plugin versions come from the version catalog;
// each is applied with `apply false` here and enabled per-module.
plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.kotlin.android) apply false
    alias(libs.plugins.kotlin.compose) apply false
    alias(libs.plugins.ksp) apply false
    // FCM (todo 30): on classpath so the app module can apply it conditionally
    // (only when app/google-services.json is present) via apply(plugin = ...).
    alias(libs.plugins.google.services) apply false
}
