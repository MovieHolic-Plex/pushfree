<#
.SYNOPSIS
    Idempotent Android toolchain bootstrap for Windows: JDK 17 + Android SDK
    (cmdline-tools + platform-tools + platforms;android-35 + build-tools;35.0.0).

.DESCRIPTION
    Designed to REUSE an existing SDK when present. ANDROID_HOME defaults to the Android
    Studio location $env:LOCALAPPDATA\Android\Sdk; if that already has platform-tools /
    build-tools / platforms, those stages are skipped and only the missing pieces
    (cmdline-tools;latest, platforms;android-35, build-tools;35.0.0) are added.

    Stages (each skipped if already satisfied, so the script is safe to re-run):
      1. JDK 17  - honor a pre-set JAVA_HOME if it is a valid JDK 17; otherwise install
                   Eclipse Temurin 17 via winget (primary) or the Adoptium zip (fallback,
                   extracted to $env:USERPROFILE\tools\jdk17).
      2. cmdline-tools - download the latest commandlinetools-win-*_latest.zip from
                   https://dl.google.com/android/repository/ (version auto-discovered from
                   the repository manifest) and extract to $ANDROID_HOME\cmdline-tools\latest.
      3. licenses - accept all SDK licenses (`sdkmanager --licenses`).
      4. packages - install platform-tools, platforms;android-35, build-tools;35.0.0
                   (skipped wholesale if all three are already on disk).
      5. env vars - persist ANDROID_HOME, JAVA_HOME, and PATH entries (User scope) and also
                   export them into the current session.

    Failure behaviour: exits non-zero with a clear stderr message if JAVA_HOME is set but
    not a valid JDK 17, or if any download/extraction/sdkmanager step fails.

    NOTE on native commands: the script runs with $ErrorActionPreference='Stop' for cmdlet
    errors, but every native exe (java / adb / sdkmanager / winget) is invoked through
    Run-Native, which scopes ErrorActionPreference='Continue' locally. Without that, a
    native command writing to stderr (e.g. `java -version`, which prints to stderr) under
    'Stop' becomes a terminating error and falsely reads as failure.

    Cache / install paths (for reference):
      - Android SDK root : $ANDROID_HOME                        (default $env:LOCALAPPDATA\Android\Sdk)
      - cmdline-tools    : $ANDROID_HOME\cmdline-tools\latest
      - Gradle cache     : $env:USERPROFILE\.gradle             (default; left untouched here)
      - JDK (zip fallback): $env:USERPROFILE\tools\jdk17
      - downloaded zips  : $env:TEMP                            (removed after extraction)

.PARAMETER AndroidHome
    Override ANDROID_HOME. Defaults to the existing $env:ANDROID_HOME, or
    $env:LOCALAPPDATA\Android\Sdk when unset (Android Studio's SDK location).

.PARAMETER JdkFallbackDir
    Where to extract the Temurin 17 zip if winget is unavailable or fails.
    Defaults to $env:USERPROFILE\tools\jdk17.

.PARAMETER SkipSdkInstall
    Stop after JDK + cmdline-tools (do not run --licenses / package install). Useful when
    only smoke-testing the bootstrap. Not used by the normal run.

.EXAMPLE
    .\scripts\setup-android.ps1
.EXAMPLE
    # Failure-mode QA: a bogus JAVA_HOME must exit non-zero with a clear message.
    $env:JAVA_HOME = 'C:\nonexistent'; .\scripts\setup-android.ps1
#>

[CmdletBinding()]
param(
    [string]$AndroidHome,
    [string]$JdkFallbackDir = (Join-Path $env:USERPROFILE 'tools\jdk17'),
    [switch]$SkipSdkInstall
)

$ErrorActionPreference = 'Stop'
# Native HTTP progress rendering is very slow on PS 5.1 for large bodies; silence it.
$ProgressPreference    = 'SilentlyContinue'

if (-not $AndroidHome) {
    $AndroidHome = if ($env:ANDROID_HOME) { $env:ANDROID_HOME } else { Join-Path $env:LOCALAPPDATA 'Android\Sdk' }
}

# ---------- helpers ----------
function Write-Info([string]$m) { [Console]::Out.WriteLine("setup-android: $m") }
function Write-Err([string]$m)  { [Console]::Error.WriteLine("setup-android: ERROR: $m") }
function Die([string]$m, [int]$code = 1) { Write-Err $m; exit $code }

# Run a native exe capturing combined stdout+stderr as text, returning { Lines, ExitCode }.
# Scoped to ErrorActionPreference='Continue' so native stderr (java -version, sdkmanager
# progress) does NOT become a terminating error under the script-wide 'Stop' preference.
function Run-Native {
    param(
        [Parameter(Mandatory)][string]$Exe,
        [string[]]$Arguments = @(),
        [string[]]$StdinLines = @()
    )
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        if ($StdinLines.Count -gt 0) {
            $lines = $StdinLines | & $Exe @Arguments 2>&1 | ForEach-Object { "$_" }
        } else {
            $lines = & $Exe @Arguments 2>&1 | ForEach-Object { "$_" }
        }
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prev
    }
    return [pscustomobject]@{ Lines = @($lines); ExitCode = $code }
}

function Test-Jdk17([string]$jdkHome) {
    if (-not $jdkHome) { return $false }
    $javaExe = Join-Path $jdkHome 'bin\java.exe'
    if (-not (Test-Path -LiteralPath $javaExe)) { return $false }
    $r = Run-Native -Exe $javaExe -Arguments @('-version')
    return (($r.Lines | Out-String) -match 'version "17\.')
}

function Set-JavaHome([string]$jdkHome) {
    $env:JAVA_HOME = $jdkHome
    [Environment]::SetEnvironmentVariable('JAVA_HOME', $jdkHome, 'User')
    Write-Info "JAVA_HOME set to '$jdkHome' (User scope + current session)."
}

function Find-Temurin17Winget {
    foreach ($root in @('C:\Program Files\Eclipse Adoptium', 'C:\Program Files (x86)\Eclipse Adoptium')) {
        if (Test-Path -LiteralPath $root) {
            $hit = Get-ChildItem -LiteralPath $root -Directory -Filter 'jdk-17*' -ErrorAction SilentlyContinue |
                Select-Object -First 1
            if ($hit) { return $hit.FullName }
        }
    }
    return $null
}

function Install-Jdk17FromZip {
    if (Test-Jdk17 $JdkFallbackDir) {
        Write-Info "JDK 17 already extracted at '$JdkFallbackDir'; reusing."
        Set-JavaHome $JdkFallbackDir
        return
    }
    $url = 'https://api.adoptium.net/v3/binary/latest/17/ga/windows/x64/jdk/hotspot/normal/eclipse'
    $zip = Join-Path $env:TEMP 'temurin17.zip'
    Write-Info "Downloading Temurin 17 zip (redirect-followed) from $url"
    try {
        Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing -TimeoutSec 600
    } catch {
        Die "Temurin 17 download failed: $($_.Exception.Message)"
    }
    if (-not (Test-Path -LiteralPath $zip)) { Die "Temurin 17 download produced no file at $zip" }
    $extract = Join-Path $env:TEMP 'temurin17-extract'
    if (Test-Path $extract) { Remove-Item $extract -Recurse -Force }
    Expand-Archive -LiteralPath $zip -DestinationPath $extract -Force
    $top = Get-ChildItem $extract -Directory | Select-Object -First 1
    if (-not $top) { Die "Temurin zip did not contain a top-level directory." }
    if (-not (Test-Jdk17 $top.FullName)) { Die "Extracted Temurin is not a valid JDK 17 at $($top.FullName)." }
    New-Item -ItemType Directory -Force -Path (Split-Path $JdkFallbackDir) | Out-Null
    if (Test-Path $JdkFallbackDir) { Remove-Item $JdkFallbackDir -Recurse -Force }
    Move-Item -LiteralPath $top.FullName -Destination $JdkFallbackDir -Force
    Remove-Item $zip -Force -ErrorAction SilentlyContinue
    Remove-Item $extract -Recurse -Force -ErrorAction SilentlyContinue
    Write-Info "Installed Temurin 17 (zip) -> '$JdkFallbackDir'."
    Set-JavaHome $JdkFallbackDir
}

function Install-Jdk17 {
    $winget = Get-Command winget.exe -ErrorAction SilentlyContinue
    if ($winget) {
        Write-Info "Trying winget install EclipseAdoptium.Temurin.17.JDK ..."
        $wr = Run-Native -Exe 'winget.exe' -Arguments @(
            'install', '--id', 'EclipseAdoptium.Temurin.17.JDK',
            '--accept-package-agreements', '--accept-source-agreements', '--silent')
        $wr.Lines | ForEach-Object { Write-Info "  winget: $_" }
        if ($wr.ExitCode -eq 0) {
            # winget-installed MSI may set a machine JAVA_HOME; refresh from the live env.
            $machineJava = [Environment]::GetEnvironmentVariable('JAVA_HOME', 'Machine')
            $candidate = if ($machineJava) { $machineJava } else { (Find-Temurin17Winget) }
            if ($candidate -and (Test-Jdk17 $candidate)) {
                Set-JavaHome $candidate
                return
            }
            Write-Info "winget reported success but no usable JDK 17 was located; falling back to zip."
        } else {
            Write-Info "winget install exited $($wr.ExitCode) (needs elevation / blocked); falling back to Adoptium zip."
        }
    } else {
        Write-Info "winget not found on PATH; falling back to Adoptium zip."
    }
    Install-Jdk17FromZip
}

function Get-LatestCmdlineToolsUrl {
    $xmlUrl = 'https://dl.google.com/android/repository/repository2-3.xml'
    Write-Info "Fetching repository manifest $xmlUrl"
    $resp = Invoke-WebRequest -Uri $xmlUrl -UseBasicParsing -TimeoutSec 60
    $text = $resp.Content
    $idx = $text.IndexOf('path="cmdline-tools;latest"')
    if ($idx -lt 0) { Die "cmdline-tools;latest package not found in manifest at $xmlUrl." }
    $block = $text.Substring($idx)
    $end = $block.IndexOf('</remotePackage>')
    if ($end -ge 0) { $block = $block.Substring(0, $end) }
    $m = [regex]::Match($block, 'commandlinetools-win-\d+_latest\.zip')
    if (-not $m.Success) { Die "No commandlinetools-win-*_latest.zip referenced for cmdline-tools;latest in manifest." }
    return ('https://dl.google.com/android/repository/' + $m.Value)
}

function Install-CmdlineTools {
    $latest = Join-Path $AndroidHome 'cmdline-tools\latest'
    if (Test-Path -LiteralPath (Join-Path $latest 'bin\sdkmanager.bat')) {
        Write-Info "cmdline-tools already present at '$latest'; skipping download."
        return
    }
    $url  = Get-LatestCmdlineToolsUrl
    $zip  = Join-Path $env:TEMP 'commandlinetools-win.zip'
    Write-Info "Downloading cmdline-tools: $url"
    try {
        Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing -TimeoutSec 600
    } catch {
        Die "cmdline-tools download failed: $($_.Exception.Message)"
    }
    if (-not (Test-Path -LiteralPath $zip)) { Die "cmdline-tools download produced no file at $zip" }
    $extract = Join-Path $env:TEMP 'cmdline-tools-extract'
    if (Test-Path $extract) { Remove-Item $extract -Recurse -Force }
    Expand-Archive -LiteralPath $zip -DestinationPath $extract -Force
    $src = Join-Path $extract 'cmdline-tools'
    if (-not (Test-Path -LiteralPath $src)) { Die "Unexpected zip layout: 'cmdline-tools\' not found after extraction." }
    New-Item -ItemType Directory -Force -Path (Join-Path $AndroidHome 'cmdline-tools') | Out-Null
    if (Test-Path $latest) { Remove-Item $latest -Recurse -Force }
    Move-Item -LiteralPath $src -Destination $latest -Force
    Remove-Item $zip -Force -ErrorAction SilentlyContinue
    Remove-Item $extract -Recurse -Force -ErrorAction SilentlyContinue
    Write-Info "Installed cmdline-tools -> '$latest'."
}

function Get-SdkManager { return (Join-Path $AndroidHome 'cmdline-tools\latest\bin\sdkmanager.bat') }

function Invoke-SdkManager([string[]]$sargs, [switch]$PipeYes) {
    $sdk = Get-SdkManager
    if (-not (Test-Path -LiteralPath $sdk)) { Die "sdkmanager not found at $sdk." }
    # sdkmanager --licenses and package installs prompt y/n repeatedly. Feed enough 'y' lines.
    $stdin = if ($PipeYes) { @('y') * 40 } else { @() }
    $r = Run-Native -Exe $sdk -Arguments $sargs -StdinLines $stdin
    $r.Lines | ForEach-Object { Write-Info "  sdkmanager: $_" }
    return $r.ExitCode
}

# Filesystem check for the idempotent reuse path: do not call sdkmanager if all three
# required packages are already on disk (e.g. an existing Android Studio SDK).
function Test-SdkPackagesInstalled {
    foreach ($p in @(
        (Join-Path $AndroidHome 'platform-tools\adb.exe'),
        (Join-Path $AndroidHome 'platforms\android-35'),
        (Join-Path $AndroidHome 'build-tools\35.0.0')
    )) {
        if (-not (Test-Path -LiteralPath $p)) { return $false }
    }
    return $true
}

function Set-EnvPersistent {
    [Environment]::SetEnvironmentVariable('ANDROID_HOME', $AndroidHome, 'User')
    $env:ANDROID_HOME = $AndroidHome
    Write-Info "ANDROID_HOME set to '$AndroidHome' (User scope + current session)."

    # PATH entries (User scope). Avoid duplicating entries already present.
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $userPath) { $userPath = '' }
    $existing = ($userPath -split ';') | ForEach-Object { $_.Trim() } | Where-Object { $_ }
    $entries  = @(
        (Join-Path $AndroidHome 'platform-tools'),
        (Join-Path $AndroidHome 'cmdline-tools\latest\bin'),
        (Join-Path $env:JAVA_HOME 'bin')
    )
    foreach ($e in $entries) {
        if (-not (Test-Path -LiteralPath $e)) { continue }
        if ($existing -notcontains $e) {
            $userPath = ($userPath.TrimEnd(';') + ';' + $e)
            $existing += $e
        }
    }
    [Environment]::SetEnvironmentVariable('Path', $userPath, 'User')
    # Export into the current session too (so callers can verify immediately).
    $env:Path = ($env:Path.TrimEnd(';') + ';' + ($entries -join ';'))
    Write-Info "PATH (User) updated with platform-tools, cmdline-tools/latest/bin, JDK bin."
}

# ---------- stage 1: JDK 17 ----------
Write-Info "Stage 1/5: JDK 17"
Write-Info "JAVA_HOME (incoming) = '$env:JAVA_HOME'"
if ($env:JAVA_HOME) {
    if (-not (Test-Jdk17 $env:JAVA_HOME)) {
        Die ("JAVA_HOME is set to '$env:JAVA_HOME' but it is not a valid JDK 17 " +
             "(bin\java.exe is missing or java -version is not 17.x). " +
             "Either point JAVA_HOME at a JDK 17 install, or clear it (`$env:JAVA_HOME=''; " +
             "[Environment]::SetEnvironmentVariable('JAVA_HOME',`$null,'User')) to let this " +
             "script install one.")
    }
    Write-Info "Using existing JAVA_HOME ('$env:JAVA_HOME')."
    Set-JavaHome $env:JAVA_HOME   # ensure User-scoped too
} else {
    Install-Jdk17
}

# Re-verify after install (stale_state guard): the chosen JAVA_HOME MUST be a working JDK 17 now.
if (-not (Test-Jdk17 $env:JAVA_HOME)) {
    Die "Post-install check failed: JAVA_HOME='$env:JAVA_HOME' is not a working JDK 17."
}
$jver = (Run-Native -Exe (Join-Path $env:JAVA_HOME 'bin\java.exe') -Arguments @('-version')).Lines | Out-String
Write-Info "JDK OK:`n$($jver.Trim())"

if ($SkipSdkInstall) { Write-Info '-SkipSdkInstall set; stopping after JDK stage.'; exit 0 }

# ---------- stage 2: cmdline-tools ----------
Write-Info 'Stage 2/5: cmdline-tools (reuses existing Android SDK at ANDROID_HOME if present)'
Install-CmdlineTools

# ---------- stage 3: licenses ----------
Write-Info 'Stage 3/5: accept SDK licenses'
[void](Invoke-SdkManager @('--licenses') -PipeYes)

# ---------- stage 4: packages (idempotent reuse path) ----------
Write-Info 'Stage 4/5: ensure SDK packages (platform-tools, android-35, build-tools;35.0.0)'
if (Test-SdkPackagesInstalled) {
    Write-Info 'All required packages already present on disk; skipping sdkmanager install.'
} else {
    $pkgs = @('platform-tools', 'platforms;android-35', 'build-tools;35.0.0')
    $rc = Invoke-SdkManager -sargs $pkgs -PipeYes
    if ($rc -ne 0) {
        Die "sdkmanager install of packages exited $rc. See output above; common cause: license not accepted or network error."
    }
}

# ---------- stage 5: persist env ----------
Write-Info 'Stage 5/5: persist ANDROID_HOME / JAVA_HOME / PATH (User scope)'
Set-EnvPersistent

# ---------- final verify (fresh, not mid-flight) ----------
Write-Info 'FINAL VERIFY'
$adb = Join-Path $AndroidHome 'platform-tools\adb.exe'
Write-Info "java -version (fresh):"
(Run-Native -Exe (Join-Path $env:JAVA_HOME 'bin\java.exe') -Arguments @('-version')).Lines |
    ForEach-Object { Write-Info "  $_" }
Write-Info "adb --version (fresh):"
if (Test-Path -LiteralPath $adb) {
    (Run-Native -Exe $adb -Arguments @('--version')).Lines | Select-Object -First 3 |
        ForEach-Object { Write-Info "  $_" }
} else {
    Die "adb not found at $adb after install."
}
Write-Info 'sdkmanager --list_installed (fresh, android-35 / build-tools;35 / platform-tools lines):'
$lr = Run-Native -Exe (Get-SdkManager) -Arguments @('--list_installed')
if ($lr.ExitCode -ne 0) {
    Write-Info "  (--list_installed unavailable (exit $($lr.ExitCode)); showing --list matches instead)"
    $lr = Run-Native -Exe (Get-SdkManager) -Arguments @('--list')
}
$lr.Lines | Where-Object { $_ -match 'android-35|build-tools;35|platform-tools' } |
    ForEach-Object { Write-Info "  $_" }

Write-Info 'DONE. New shells will see ANDROID_HOME/JAVA_HOME/PATH automatically.'
Write-Info 'A future Gradle session needs: JAVA_HOME and ANDROID_HOME set (both User-scoped now),'
Write-Info 'plus platform-tools + cmdline-tools/latest/bin on PATH. Open a NEW shell or run:'
Write-Info ("  `$env:JAVA_HOME='$env:JAVA_HOME'; `$env:ANDROID_HOME='$env:ANDROID_HOME'; " +
    "`$env:Path=[Environment]::GetEnvironmentVariable('Path','User') + ';' + `$env:Path")
exit 0
