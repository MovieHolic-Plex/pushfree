#!/usr/bin/env bash
#
# setup-android.sh - Idempotent Android toolchain bootstrap for Linux/macOS.
#
# Installs / verifies:
#   - JDK 17 (Temurin/Zulu): prefer SDKMAN! on macOS & desktop Linux; apt for Debian/Ubuntu;
#     falls back to extracting the Adoptium tarball to $HOME/tools/jdk17.
#   - Android cmdline-tools (latest) -> $ANDROID_HOME/cmdline-tools/latest
#   - SDK licenses accepted
#   - platform-tools, platforms;android-35, build-tools;35.0.0
#   - ANDROID_HOME / JAVA_HOME exported, plus a snippet appended to the shell rc file.
#
# Safe to re-run: each stage is skipped if already satisfied.
#
# Cache / install paths (for reference):
#   - Android SDK root : $ANDROID_HOME                    (default $HOME/Android/Sdk)
#   - cmdline-tools    : $ANDROID_HOME/cmdline-tools/latest
#   - Gradle cache     : $HOME/.gradle                    (default; left untouched here)
#   - JDK (tarball fb) : $HOME/tools/jdk17
#   - downloaded tarballs/zip: $TMPDIR (removed after extraction)
#
# Usage:
#   ./scripts/setup-android.sh
# Failure-mode QA:
#   JAVA_HOME=/nonexistent ./scripts/setup-android.sh    # must exit non-zero with a clear error
set -euo pipefail

log()  { printf '%s\n' "setup-android: $*"; }
err()  { printf '%s\n' "setup-android: ERROR: $*" >&2; }
die()  { err "$*"; exit "${2:-1}"; }

ANDROID_HOME="${ANDROID_HOME:-$HOME/Android/Sdk}"
JDK_FALLBACK_DIR="${JDK_FALLBACK_DIR:-$HOME/tools/jdk17}"
PLATFORM="$(uname -s)"
ARCH="$(uname -m)"

case "$PLATFORM" in
  Linux*)  OS_TAG=linux;  RC_FILES=("$HOME/.bashrc" "$HOME/.profile") ;;
  Darwin*) OS_TAG=mac;    RC_FILES=("$HOME/.zshrc"  "$HOME/.bash_profile" "$HOME/.profile") ;;
  *)       die "Unsupported platform: $PLATFORM" ;;
esac

# --------- helpers ---------
jdk_ok() {
  local home="$1"
  [ -n "$home" ] && [ -x "$home/bin/java" ] && \
    "$home/bin/java" -version 2>&1 | grep -q 'version "17\.'
}

set_java_home() {
  local home="$1"
  export JAVA_HOME="$home"
  # Persist for future shells via rc files (idempotent export line).
  for rc in "${RC_FILES[@]}"; do
    [ -f "$rc" ] || continue
    if ! grep -q "export JAVA_HOME=" "$rc" 2>/dev/null; then
      printf 'export JAVA_HOME="%s"\n' "$home" >> "$rc"
    else
      sed -i.bak -E "s|^export JAVA_HOME=.*|export JAVA_HOME=\"$home\"|" "$rc" && rm -f "$rc.bak"
    fi
  done
  log "JAVA_HOME = $home (exported + persisted to rc files)"
}

install_jdk_apt() {
  log "Installing temurin-17-jdk via Adoptium apt repo..."
  if ! command -v wget >/dev/null 2>&1; then die "wget required for apt setup"; fi
  sudo mkdir -p /etc/apt/keyrings
  wget -qO - https://packages.adoptium.net/artifactory/api/gpg/key/public | \
    sudo tee /etc/apt/keyrings/adoptium.asc >/dev/null
  echo "deb [signed-by=/etc/apt/keyrings/adoptium.asc] https://packages.adoptium.net/artifactory/deb $(. /etc/os-release; echo "$VERSION_CODENAME") main" \
    | sudo tee /etc/apt/sources.list.d/adoptium.list >/dev/null
  sudo apt-get update -y
  sudo apt-get install -y temurin-17-jdk
  # temurin-17-jdk drops a JAVA_HOME link at /usr/lib/jvm/temurin-17-*/ ; pick it up.
  local found
  found="$(ls -d /usr/lib/jvm/temurin-17-* 2>/dev/null | head -n1 || true)"
  [ -n "$found" ] || die "apt install finished but no /usr/lib/jvm/temurin-17-* found."
  set_java_home "$found"
}

install_jdk_sdkman() {
  log "Installing JDK 17 via SDKMAN!..."
  if ! command -v sdk >/dev/null 2>&1; then
    log "SDKMAN! not found; installing it to $HOME/.sdkman"
    curl -sSL "https://get.sdkman.io" | bash || die "SDKMAN! bootstrap failed."
    export SDKMAN_DIR="$HOME/.sdkman"
    # shellcheck disable=SC1091
    source "$SDKMAN_DIR/bin/sdkman-init.sh"
  fi
  sdk install java 17.0.*/-tem < /dev/null || die "sdk install java 17 (tem) failed."
  local found
  found="$(ls -d "$HOME/.sdkman/candidates/java/17."* 2>/dev/null | head -n1 || true)"
  [ -n "$found" ] || die "sdk install finished but JAVA_HOME not located."
  set_java_home "$found"
}

install_jdk_zip() {
  if jdk_ok "$JDK_FALLBACK_DIR"; then
    log "JDK 17 already extracted at $JDK_FALLBACK_DIR; reusing."
    set_java_home "$JDK_FALLBACK_DIR"; return
  fi
  local api_arch="$ARCH"
  case "$ARCH" in x86_64|amd64) api_arch=x64 ;; aarch64|arm64) api_arch=arm ;; esac
  local url="https://api.adoptium.net/v3/binary/latest/17/ga/${OS_TAG}/${api_arch}/jdk/hotspot/normal/eclipse"
  local tmp; tmp="$(mktemp -d)"
  local tarball="$tmp/jdk17.tar.gz"
  log "Downloading Temurin 17 tarball: $url"
  curl -fsSL "$url" -o "$tarball" || die "Temurin 17 download failed: $url"
  mkdir -p "$JDK_FALLBACK_DIR"
  tar -xzf "$tarball" -C "$tmp"
  local top; top="$(ls -d "$tmp"/jdk-17* 2>/dev/null | head -n1 || true)"
  [ -n "$top" ] || die "Temurin tarball did not contain a jdk-17* directory."
  rm -rf "$JDK_FALLBACK_DIR"
  mv "$top" "$JDK_FALLBACK_DIR"
  rm -rf "$tmp"
  jdk_ok "$JDK_FALLBACK_DIR" || die "Extracted JDK at $JDK_FALLBACK_DIR is not a working JDK 17."
  log "Installed Temurin 17 -> $JDK_FALLBACK_DIR"
  set_java_home "$JDK_FALLBACK_DIR"
}

install_jdk() {
  # Prefer SDKMAN! on macOS, apt on Debian/Ubuntu; both fall back to the zip.
  if [ "$OS_TAG" = mac ] && command -v brew >/dev/null 2>&1; then
    if command -v sdk >/dev/null 2>&1 || command -v curl >/dev/null 2>&1; then
      install_jdk_sdkman && return 0
      log "SDKMAN! path failed; falling back to zip."
    fi
  fi
  if [ "$OS_TAG" = linux ] && command -v apt-get >/dev/null 2>&1; then
    install_jdk_apt && return 0
    log "apt path failed; falling back to zip."
  fi
  install_jdk_zip
}

latest_cmdline_tools_url() {
  local manifest="https://dl.google.com/android/repository/repository2-3.xml"
  log "Fetching repository manifest $manifest"
  local block url
  block="$(curl -fsSL "$manifest")" || die "failed to download repository manifest"
  # Isolate the cmdline-tools;latest package and its <OS_TAG> archive url.
  block="${block#*path=\"cmdline-tools;latest\"}"
  block="${block%%</remotePackage>*}"
  url="$(printf '%s' "$block" | grep -oE "commandlinetools-${OS_TAG}-[0-9]+_latest.zip" | head -n1 || true)"
  [ -n "$url" ] || die "no commandlinetools-${OS_TAG}-*_latest.zip found in manifest."
  echo "https://dl.google.com/android/repository/$url"
}

install_cmdline_tools() {
  local latest="$ANDROID_HOME/cmdline-tools/latest"
  if [ -x "$latest/bin/sdkmanager" ]; then
    log "cmdline-tools already present at $latest; skipping."
    return
  fi
  local url zip tmp top
  url="$(latest_cmdline_tools_url)"
  tmp="$(mktemp -d)"; zip="$tmp/cmdline-tools.zip"
  log "Downloading cmdline-tools: $url"
  curl -fsSL "$url" -o "$zip" || die "cmdline-tools download failed: $url"
  mkdir -p "$ANDROID_HOME/cmdline-tools"
  tar -xzf "$zip" -C "$tmp" || unzip -q "$zip" -d "$tmp"
  top="$tmp/cmdline-tools"
  [ -d "$top" ] || die "unexpected zip layout: 'cmdline-tools' dir missing after extract."
  rm -rf "$latest"
  mv "$top" "$latest"
  rm -rf "$tmp"
  log "Installed cmdline-tools -> $latest"
}

sdkmanager_bin() { echo "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager"; }

sdkmanager_run() {
  local sdk; sdk="$(sdkmanager_bin)"
  [ -x "$sdk" ] || die "sdkmanager not found at $sdk."
  yes | "$sdk" "$@" || true
}

set_env_persistent() {
  local sdk_bin="$ANDROID_HOME/cmdline-tools/latest/bin"
  local ptools="$ANDROID_HOME/platform-tools"
  local rc_line="export ANDROID_HOME=\"$ANDROID_HOME\""
  for rc in "${RC_FILES[@]}"; do
    [ -f "$rc" ] || continue
    if ! grep -q "export ANDROID_HOME=" "$rc" 2>/dev/null; then
      printf 'export ANDROID_HOME="%s"\n' "$ANDROID_HOME" >> "$rc"
    else
      sed -i.bak -E "s|^export ANDROID_HOME=.*|export ANDROID_HOME=\"$ANDROID_HOME\"|" "$rc" && rm -f "$rc.bak"
    fi
    # Prepend the SDK bin dirs to PATH if not already referenced.
    if ! grep -q "ANDROID_HOME/cmdline-tools/latest/bin" "$rc" 2>/dev/null; then
      printf 'case ":$PATH:" in *"%s"*) ;; *) export PATH="%s:$PATH";; esac\n' "$ptools" "$ptools" >> "$rc"
      printf 'case ":$PATH:" in *"%s"*) ;; *) export PATH="%s:$PATH";; esac\n' "$sdk_bin" "$sdk_bin" >> "$rc"
    fi
  done
  export ANDROID_HOME
  export PATH="$ptools:$sdk_bin:$PATH"
  log "ANDROID_HOME + PATH persisted to rc files (${RC_FILES[*]})."
}

# --------- stage 1: JDK 17 ---------
log "Stage 1/5: JDK 17"
log "JAVA_HOME (incoming) = ${JAVA_HOME:-<unset>}"
if [ -n "${JAVA_HOME:-}" ]; then
  if ! jdk_ok "$JAVA_HOME"; then
    die "JAVA_HOME is set to '$JAVA_HOME' but it is not a valid JDK 17 (bin/java missing or wrong version). Point JAVA_HOME at a JDK 17, or unset it to let this script install one."
  fi
  log "Using existing JAVA_HOME ($JAVA_HOME)."
  set_java_home "$JAVA_HOME"
else
  install_jdk
fi
jdk_ok "$JAVA_HOME" || die "Post-install check failed: JAVA_HOME=$JAVA_HOME is not a working JDK 17."
log "JDK OK: $("$JAVA_HOME/bin/java" -version 2>&1 | head -n1)"

# --------- stage 2: cmdline-tools ---------
log "Stage 2/5: cmdline-tools"
install_cmdline_tools

# --------- stage 3: licenses ---------
log "Stage 3/5: accept SDK licenses"
sdkmanager_run --licenses

# --------- stage 4: packages (idempotent reuse path) ---------
sdk_packages_installed() {
  [ -x "$ANDROID_HOME/platform-tools/adb" ] && \
  [ -d "$ANDROID_HOME/platforms/android-35" ] && \
  [ -d "$ANDROID_HOME/build-tools/35.0.0" ]
}
log "Stage 4/5: ensure SDK packages (platform-tools, android-35, build-tools;35.0.0)"
if sdk_packages_installed; then
  log "All required packages already present on disk; skipping sdkmanager install."
else
  if ! yes | "$(sdkmanager_bin)" "platform-tools" "platforms;android-35" "build-tools;35.0.0"; then
    die "sdkmanager package install failed (license not accepted or network error)."
  fi
fi

# --------- stage 5: persist env ---------
log "Stage 5/5: persist ANDROID_HOME / JAVA_HOME / PATH"
set_env_persistent

# --------- final verify ---------
log "FINAL VERIFY"
log "java -version (fresh):"; "$JAVA_HOME/bin/java" -version 2>&1 | sed 's/^/  /'
log "adb --version (fresh):"; "$ANDROID_HOME/platform-tools/adb" --version 2>&1 | head -n3 | sed 's/^/  /'
log "sdkmanager --list_installed (fresh):"; sdkmanager_run --list_installed | sed 's/^/  /'
log "DONE. Open a new shell or: export ANDROID_HOME=\"$ANDROID_HOME\" JAVA_HOME=\"$JAVA_HOME\""
