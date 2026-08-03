plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.ksp)
    alias(libs.plugins.paparazzi)
}

android {
    namespace = "net.pushfree.android"
    compileSdk = 35

    defaultConfig {
        applicationId = "net.pushfree.android"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"

        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro",
            )
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    // Robolectric needs merged resources to build a working Android Context on JVM
    // (required for Room.inMemoryDatabaseBuilder in plain unit tests).
    testOptions {
        unitTests {
            isIncludeAndroidResources = true
        }
    }

    // Kotlin toolchain: compile Kotlin with JDK 17 (matches daemon JVM).
    kotlin {
        jvmToolchain(17)
    }

    buildFeatures {
        compose = true
    }

    packaging {
        resources {
            excludes += "/META-INF/{AL2.0,LGPL2.1}"
        }
    }
}

dependencies {
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.lifecycle.runtime.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.ui)
    implementation(libs.androidx.ui.graphics)
    implementation(libs.androidx.ui.tooling.preview)
    implementation(libs.androidx.material3)
    debugImplementation(libs.androidx.ui.tooling)

    // Compose UI (todo 34): ViewModel integration + lifecycle-aware state collection.
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)

    // Room data layer (compiler processed by KSP to match Kotlin 2.0.21)
    implementation(libs.androidx.room.runtime)
    implementation(libs.androidx.room.ktx)
    ksp(libs.androidx.room.compiler)

    // Resilient ack outbox (todo 33): network-constrained retried ack POSTs.
    implementation(libs.androidx.work.runtime.ktx)

    // WebSocket transport (todo 29): OkHttp provides the WS client + timeouts.
    implementation(libs.okhttp)

    // Optional FCM transport (todo 30): firebase-messaging is always on the
    // compile classpath so FirebaseMessagingService resolves; the channel is
    // runtime-disabled when google-services.json is absent (FirebaseApp is not
    // auto-initialized). The google-services plugin is applied conditionally
    // at the end of this file so the build succeeds without that file.
    implementation(platform(libs.firebase.bom))
    implementation(libs.firebase.messaging)

    testImplementation(libs.junit)
    testImplementation(libs.androidx.room.testing)
    testImplementation(libs.robolectric)
    testImplementation(libs.androidx.test.core)
    testImplementation(libs.androidx.test.ext.junit)
    testImplementation(libs.kotlinx.coroutines.test)
    testImplementation(libs.androidx.work.testing)
    // MockWebServer drives the WS transport against a scripted in-process server.
    testImplementation(libs.mockwebserver)
}

// FCM (todo 30): apply the google-services plugin ONLY when a config file is
// present. Absent -> build still succeeds and the channel stays runtime-
// disabled (FirebaseApp is not auto-initialized, so FirebaseMessagingService
// is never invoked). Present -> the plugin emits FirebaseOptions resources so
// the transport activates at runtime. The plugin block cannot itself be
// conditional, hence the post-block apply().
if (file("google-services.json").exists()) {
    apply(plugin = "com.google.gms.google-services")
}
