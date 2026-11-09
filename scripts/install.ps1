# Install Habakkuk on Windows from its GitHub releases.
#
#   irm https://habakkuk.ai/install.ps1 | iex
#
# or, with options, save it and run it:
#
#   irm https://habakkuk.ai/install.ps1 -OutFile install.ps1
#   .\install.ps1 -Version v0.4.0 -Dir C:\Tools\Habakkuk
#
# Options (also read from HABAKKUK_VERSION, HABAKKUK_INSTALL_DIR and
# HABAKKUK_DOWNLOAD_URL, which is how to set them with `| iex`):
#   -Version TAG   install this release, e.g. v0.4.0 (default: the latest)
#   -Dir DIR       install into DIR (default: %LOCALAPPDATA%\Programs\Habakkuk)
#   -NoPath        do not add DIR to your user PATH
#
# It downloads habakkuk_windows_<arch>.zip and SHA256SUMS, refuses an archive
# whose SHA-256 does not match, and puts habakkuk.exe in DIR. No admin rights
# are needed. Habakkuk keeps its data in %APPDATA%\Habakkuk ($env:HABAKKUK_HOME
# overrides it); later, `habakkuk update` upgrades in place.

param(
    [string]$Version = $env:HABAKKUK_VERSION,
    [string]$Dir = $env:HABAKKUK_INSTALL_DIR,
    [switch]$NoPath
)

function Install-Habakkuk {
    param([string]$Version, [string]$Dir, [bool]$NoPath)

    # Errors throw rather than exit: under `irm | iex`, exit would close the
    # user's PowerShell window.
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is far faster without the progress bar
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    } catch { }

    $repo = 'habakkuk-ai/habakkuk'
    if (-not $Version) { $Version = 'latest' }
    if (-not $Dir) { $Dir = Join-Path $env:LOCALAPPDATA 'Programs\Habakkuk' }

    $arch = $null
    try {
        $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    } catch {
        $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    }
    switch -Regex ($arch) {
        '^(X64|AMD64)$' { $arch = 'amd64' }
        '^ARM64$' { $arch = 'arm64' }
        default { throw "habakkuk install: no Windows release for $arch; build from source with: go install github.com/$repo/cmd/habakkuk@latest" }
    }

    if ($env:HABAKKUK_DOWNLOAD_URL) {
        $base = $env:HABAKKUK_DOWNLOAD_URL.TrimEnd('/')
    } elseif ($Version -eq 'latest') {
        $base = "https://github.com/$repo/releases/latest/download"
    } else {
        $base = "https://github.com/$repo/releases/download/$Version"
    }
    $archive = "habakkuk_windows_$arch.zip"

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("habakkuk-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Downloading Habakkuk ($Version, windows/$arch)..."
        $zip = Join-Path $tmp $archive
        $sums = Join-Path $tmp 'SHA256SUMS'
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile $zip
            Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $sums
        } catch {
            throw "habakkuk install: could not download $base/$archive or its SHA256SUMS: $($_.Exception.Message)"
        }

        $want = $null
        foreach ($line in Get-Content -LiteralPath $sums) {
            $f = -split $line
            if ($f.Count -eq 2 -and ($f[1] -eq $archive -or $f[1] -eq "*$archive")) { $want = $f[0].ToLowerInvariant() }
        }
        if (-not $want) { throw "habakkuk install: SHA256SUMS has no entry for $archive" }
        $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash.ToLowerInvariant()
        if ($got -ne $want) { throw "habakkuk install: checksum mismatch for $archive (want $want, got $got); not installing" }
        Write-Host 'Checksum verified.'

        $x = Join-Path $tmp 'x'
        Expand-Archive -LiteralPath $zip -DestinationPath $x -Force
        $new = Join-Path $x 'habakkuk.exe'
        if (-not (Test-Path -LiteralPath $new)) { throw "habakkuk install: $archive does not contain habakkuk.exe" }

        New-Item -ItemType Directory -Force -Path $Dir | Out-Null
        $exe = Join-Path $Dir 'habakkuk.exe'
        # A running habakkuk.exe cannot be overwritten, but it can be moved
        # aside; Habakkuk deletes the .old file the next time it starts.
        $staged = Join-Path $Dir '.habakkuk.exe.new'
        Copy-Item -LiteralPath $new -Destination $staged -Force
        if (Test-Path -LiteralPath $exe) {
            $old = "$exe.old"
            Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
            if (Test-Path -LiteralPath $old) { $old = "$exe.old-" + [DateTime]::UtcNow.ToString('yyyyMMddHHmmss') }
            Move-Item -LiteralPath $exe -Destination $old -Force
            try {
                Move-Item -LiteralPath $staged -Destination $exe -Force
            } catch {
                Move-Item -LiteralPath $old -Destination $exe -Force
                throw
            }
            Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
        } else {
            Move-Item -LiteralPath $staged -Destination $exe -Force
        }

        $installed = (& $exe version 2>$null | Select-Object -First 1)
        if (-not $installed) { $installed = 'habakkuk' }
        Write-Host "Installed $installed to $exe"

        if (-not $NoPath) {
            $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
            $parts = @()
            if ($userPath) { $parts = $userPath -split ';' | Where-Object { $_ } }
            if (-not ($parts | Where-Object { $_.TrimEnd('\') -ieq $Dir.TrimEnd('\') })) {
                [Environment]::SetEnvironmentVariable('Path', (($parts + $Dir) -join ';'), 'User')
                Write-Host "Added $Dir to your user PATH (new terminals pick it up)."
            }
            if (-not (($env:Path -split ';') | Where-Object { $_.TrimEnd('\') -ieq $Dir.TrimEnd('\') })) {
                $env:Path = "$env:Path;$Dir"
            }
        }
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }

    Write-Host ''
    Write-Host 'Next steps:'
    Write-Host '  habakkuk init                 # writes %APPDATA%\Habakkuk\config.yaml'
    Write-Host '  notepad $env:APPDATA\Habakkuk\.env   # add ANTHROPIC_API_KEY=... (or another provider''s key)'
    Write-Host '  habakkuk doctor               # checks config, keys and channels'
    Write-Host '  habakkuk chat'
}

Install-Habakkuk -Version $Version -Dir $Dir -NoPath:$NoPath.IsPresent
