# Pushfree Android - Release Signing & F-Droid

This documents how the Android release variant is signed and how to publish a
release. There is intentionally **no release keystore committed** to this repo;
production signing material is supplied through environment variables only.

## Versioning (single source of truth)

`versionCode` / `versionName` are declared **once** in
[`gradle.properties`](./gradle.properties):

```properties
pushfree.versionCode=1
pushfree.versionName=0.1.0
```

These are consumed by:

- [`app/build.gradle.kts`](./app/build.gradle.kts) (`defaultConfig`)
- [`metadata/net.pushfree.android.yml`](./metadata/net.pushfree.android.yml)
  (the F-Droid listing mirror)
- this file

To cut a release: bump the two values in `gradle.properties` and update the
F-Droid yml mirror (`CurrentVersion`, `CurrentVersionCode`, and the matching
`Builds:` entry) **in the same commit**.

## Release signing configuration

`app/build.gradle.kts` defines a `release` signing config that reads four
environment variables:

| Variable                    | Purpose                          |
|-----------------------------|----------------------------------|
| `PUSHFREE_KEYSTORE`         | Path to the `.keystore` / `.jks` |
| `PUSHFREE_KEYSTORE_PASSWORD`| Keystore password                |
| `PUSHFREE_KEY_ALIAS`        | Key alias inside the keystore    |
| `PUSHFREE_KEY_PASSWORD`     | Password for that key            |

### Happy path (production-signed release)

```bash
export PUSHFREE_KEYSTORE=$HOME/.pushfree/release.jks
export PUSHFREE_KEYSTORE_PASSWORD=...
export PUSHFREE_KEY_ALIAS=pushfree
export PUSHFREE_KEY_PASSWORD=...
cd android
./gradlew assembleRelease
# -> app/build/outputs/apk/release/app-release.apk  (production-signed)
```

### Failure / missing-keystore path (the documented behavior)

If **any** of the four variables is unset (CI, F-Droid reproducible builds,
local development), the build does **not** fail. It transparently falls back
to the AGP-managed **debug keystore** (`~/.android/debug.keystore`) so that
`./gradlew assembleRelease` always produces an installable APK:

```bash
unset PUSHFREE_KEYSTORE PUSHFREE_KEYSTORE_PASSWORD PUSHFREE_KEY_ALIAS PUSHFREE_KEY_PASSWORD
cd android
./gradlew assembleRelease
# -> app/build/outputs/apk/release/app-release.apk  (DEBUG-signed, NOT publishable)
```

A debug-signed APK is fine for QA, screenshots, and F-Droid's own rebuild
(F-Droid signs with its own key), but it **MUST NEVER be uploaded to a store**.
The release workflow is responsible for gating on the env vars before any
upload step.

To verify which key signed an APK:

```bash
$ANDROID_HOME/build-tools/35.0.0/apksigner verify --print-certs \
    app/build/outputs/apk/release/app-release.apk
```

The debug fallback prints a certificate whose subject is
`CN=Android Debug,O=Android,C=US`; a production build prints your release
certificate. Inspect the build log lines or `apksigner` output to distinguish
the two.

## Creating a release keystore (one-time)

```bash
keytool -genkeypair -v \
  -keystore release.jks \
  -alias pushfree \
  -keyalg RSA -keysize 4096 -validity 10000 \
  -storepass <STOREPASS> -keypass <KEYPASS>
```

Keep `release.jks` and both passwords in your secrets manager. Never commit
the keystore to git (it is in `.gitignore`-worthy: do not place it under the
repo tree).

## F-Droid metadata

The in-repo reference metadata is
[`metadata/net.pushfree.android.yml`](./metadata/net.pushfree.android.yml).
The F-Droid-conventional published location is
`metadata/<applicationId>.yml` inside the `fdroid-data` repository; this file
is what the maintainer mirrors there per release.

Validate locally:

```bash
python -c "import yaml,sys; yaml.safe_load(open('metadata/net.pushfree.android.yml')); print('yml ok')"
```

## fastlane metadata

Store-listing copy and the icon live under
[`fastlane/metadata/android/en-US/`](./fastlane/metadata/android/en-US/).
Screenshots under `images/phoneScreenshots/` are **placeholders** - capture
real screenshots from a release build before any listing submission (see the
README in that directory).
