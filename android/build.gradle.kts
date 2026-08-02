// Top-level build file. Plugin versions come from the version catalog;
// each is applied with `apply false` here and enabled per-module.
plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.kotlin.android) apply false
    alias(libs.plugins.kotlin.compose) apply false
}
