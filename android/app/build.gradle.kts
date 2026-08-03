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

    // ---- Versioning (single source: android/gradle.properties) ----
    // versionCode/versionName live ONLY in gradle.properties
    // (pushfree.versionCode / pushfree.versionName) so the F-Droid metadata
    // (android/metadata/net.pushfree.android.yml) and release tooling read a
    // single value. Bump there and update the yml mirror in the same commit.
    val pushfreeVersionCode = (project.property("pushfree.versionCode") as String).toInt()
    val pushfreeVersionName = project.property("pushfree.versionName") as String

    defaultConfig {
        applicationId = "net.pushfree.android"
        minSdk = 26
        targetSdk = 35
        versionCode = pushfreeVersionCode
        versionName = pushfreeVersionName

        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    // ---- Release signing ----
    // A production release is signed with a keystore supplied through env vars:
    //   PUSHFREE_KEYSTORE          - path to the .keystore/.jks file
    //   PUSHFREE_KEYSTORE_PASSWORD - keystore password
    //   PUSHFREE_KEY_ALIAS         - key alias inside the keystore
    //   PUSHFREE_KEY_PASSWORD      - password for that key
    // When ANY of these is unset (CI, F-Droid reproducible builds, local dev)
    // the release variant transparently falls back to the AGP-managed debug
    // keystore (~/.android/debug.keystore) so `./gradlew assembleRelease`
    // ALWAYS produces an installable APK and the build never fails on missing
    // secrets. A debug-signed release MUST NEVER be published to a store; the
    // release workflow gates on the env vars before upload. See
    // android/RELEASING.md. To verify which key signed an APK:
    //   apksigner verify --print-certs app/build/outputs/apk/release/app-release.apk
    val pushfreeKeystorePath = providers.environmentVariable("PUSHFREE_KEYSTORE").orNull
    val pushfreeKeystorePassword = providers.environmentVariable("PUSHFREE_KEYSTORE_PASSWORD").orNull
    val pushfreeKeyAlias = providers.environmentVariable("PUSHFREE_KEY_ALIAS").orNull
    val pushfreeKeyPassword = providers.environmentVariable("PUSHFREE_KEY_PASSWORD").orNull

    signingConfigs {
        create("release") {
            if (pushfreeKeystorePath != null &&
                pushfreeKeystorePassword != null &&
                pushfreeKeyAlias != null &&
                pushfreeKeyPassword != null) {
                storeFile = file(pushfreeKeystorePath)
                storePassword = pushfreeKeystorePassword
                keyAlias = pushfreeKeyAlias
                keyPassword = pushfreeKeyPassword
            } else {
                // Fallback: reuse the debug keystore (auto-managed by AGP).
                // Keeps the release variant buildable/installable w/o secrets.
                val debug = signingConfigs.getByName("debug")
                storeFile = debug.storeFile
                storePassword = debug.storePassword
                keyAlias = debug.keyAlias
                keyPassword = debug.keyPassword
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            signingConfig = signingConfigs.getByName("release")
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro",
            )
        }
    }

    // ---- Product flavors (todo 49: Play Store pipeline) ----
    // Two store-channel flavors share one Gradle project. The split mirrors
    // the F-Droid purity rule and its inverse:
    //   play   - Play Store build. Carries Firebase Cloud Messaging (Google)
    //            and does NOT register the UnifiedPush receiver (the F-Droid
    //            flavor is Google-free + UnifiedPush, so Play inverts that:
    //            Google push, no UP). firebase-messaging is a PLAY-ONLY
    //            dependency (playImplementation below); only the `play` source
    //            set contains code that imports Firebase (PushfreeFcmService),
    //            so the fdroid variant has ZERO Google artifacts on its
    //            classpath. applicationId gets a ".play" suffix so the two
    //            builds coexist on one device.
    //   fdroid  - F-Droid / Google-free build. Registers the UnifiedPush
    //            receiver (src/fdroid), no Firebase dependency.
    // Both flavors build WITHOUT google-services.json: absent -> the play
    // flavor still compiles (firebase-messaging is on its classpath) but the
    // channel stays runtime-disabled (FirebaseApp never auto-initializes).
    flavorDimensions += "store"
    productFlavors {
        create("play") {
            dimension = "store"
            applicationIdSuffix = ".play"
            // play applicationId -> net.pushfree.android.play
            // (the task spec named "com.pushfree.android.play"; the base
            // applicationId is net.pushfree.android per todo 27 + the F-Droid
            // metadata at metadata/net.pushfree.android.yml, so the suffix
            // composes on that established base. See android/RELEASING.md.)
        }
        create("fdroid") {
            dimension = "store"
            // No suffix: applicationId stays net.pushfree.android, matching
            // metadata/net.pushfree.android.yml (F-Droid listing).
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

    // FCM transport (todo 30/49): Firebase is a PLAY-FLAVOR-ONLY dependency.
    // The `play` flavor carries FCM (Google); the `fdroid` flavor is
    // Google-free and uses UnifiedPush instead (see src/fdroid). Only the
    // play source set contains code that imports Firebase
    // (PushfreeFcmService), so the fdroid variant compiles with no Firebase
    // on its classpath. The google-services plugin is applied conditionally
    // at the end of this file (only when google-services.json is present) so
    // the play flavor still builds with the channel runtime-disabled when
    // that file is absent.
    "playImplementation"(platform(libs.firebase.bom))
    "playImplementation"(libs.firebase.messaging)

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

// FCM (todo 30/49): apply the google-services plugin ONLY when a config file
// is present. The plugin is project-wide but only the `play` flavor uses
// Firebase; in practice google-services.json is supplied exclusively for Play
// builds, so the F-Droid/Google-free build (no file present) is unaffected.
// Absent -> the play flavor still builds and the channel stays runtime-
// disabled (FirebaseApp is not auto-initialized, so PushfreeFcmService is
// never invoked). Present -> the plugin emits FirebaseOptions resources so
// the transport activates at runtime. The plugins{} block cannot itself be
// conditional, hence the post-block apply().
if (file("google-services.json").exists()) {
    apply(plugin = "com.google.gms.google-services")
}
