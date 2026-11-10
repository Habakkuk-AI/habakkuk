# Tests for the parts of install.ps1 that need no network and no Windows:
# it parses, and its two helpers (the PATH list and the "is this hk ours"
# check) behave. CI runs this in PowerShell 7 and Windows PowerShell 5.1;
# it also runs in pwsh on Linux and macOS:
#
#   pwsh -NoProfile -File scripts/install_test.ps1
#
# The helpers are read out of install.ps1 as written, so nothing here can
# drift from the script people run.

$ErrorActionPreference = 'Stop'
$script = Join-Path $PSScriptRoot 'install.ps1'
$failed = 0
function Check([string]$Name, [bool]$Ok) {
    if ($Ok) { Write-Host "ok    $Name" } else { $script:failed++; Write-Host "FAIL  $Name" }
}

$errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($script, [ref]$null, [ref]$errors)
Check 'install.ps1 parses' (-not $errors)
if ($errors) { $errors | Format-List; exit 1 }

$fns = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)
foreach ($f in $fns) {
    if ($f.Name -ne 'Install-Habakkuk') { Invoke-Expression $f.Extent.Text }
}

# The PATH list. A clean Windows account has exactly one entry in its user
# PATH, with a trailing semicolon; that case once glued two folders together.
$hab = 'C:\Users\me\AppData\Local\Programs\Habakkuk'
$apps = 'C:\Users\me\AppData\Local\Microsoft\WindowsApps'
Check 'one entry with a trailing semicolon' ((Add-HabakkukPathEntry -List "$apps;" -Dir $hab) -ceq "$apps;$hab")
Check 'one entry' ((Add-HabakkukPathEntry -List $apps -Dir $hab) -ceq "$apps;$hab")
Check 'two entries' ((Add-HabakkukPathEntry -List 'C:\a;C:\b' -Dir $hab) -ceq "C:\a;C:\b;$hab")
Check 'an empty list' ((Add-HabakkukPathEntry -List '' -Dir $hab) -ceq $hab)
Check 'no list at all' ((Add-HabakkukPathEntry -List $null -Dir $hab) -ceq $hab)
Check 'empty items are dropped' ((Add-HabakkukPathEntry -List ';C:\a;;' -Dir $hab) -ceq "C:\a;$hab")
Check 'already there' ($null -eq (Add-HabakkukPathEntry -List "C:\a;$hab" -Dir $hab))
Check 'already there in another case, with a trailing backslash' ($null -eq (Add-HabakkukPathEntry -List ("C:\a;" + $hab.ToUpper() + '\') -Dir $hab))
Check 'a longer folder with the same start is not a match' ((Add-HabakkukPathEntry -List "$hab-old" -Dir $hab) -ceq "$hab-old;$hab")

# Whose hk is it.
$shim = '@"%~dp0habakkuk.exe" %*'
$root = Join-Path ([IO.Path]::GetTempPath()) ('habakkuk-install-test-' + [Guid]::NewGuid().ToString('N'))
function New-TestDir([string]$Name) {
    $d = Join-Path $root $Name
    New-Item -ItemType Directory -Path $d | Out-Null
    return $d
}
try {
    New-Item -ItemType Directory -Path $root | Out-Null
    $d = New-TestDir 'empty'
    Check 'an empty folder has no other hk' (-not (Test-HabakkukForeignHk -Dir $d -Shim $shim))
    $d = New-TestDir 'ours'
    Set-Content -LiteralPath (Join-Path $d 'hk.cmd') -Value $shim -Encoding ASCII
    Check 'the hk.cmd this script writes is ours' (-not (Test-HabakkukForeignHk -Dir $d -Shim $shim))
    $d = New-TestDir 'crlf'
    [IO.File]::WriteAllText((Join-Path $d 'hk.cmd'), "$shim`r`n`r`n")
    Check 'ours with blank lines after it is still ours' (-not (Test-HabakkukForeignHk -Dir $d -Shim $shim))
    $d = New-TestDir 'other'
    Set-Content -LiteralPath (Join-Path $d 'hk.cmd') -Value '@echo other' -Encoding ASCII
    Check 'another hk.cmd is not ours' (Test-HabakkukForeignHk -Dir $d -Shim $shim)
    $d = New-TestDir 'blank'
    [IO.File]::WriteAllText((Join-Path $d 'hk.cmd'), '')
    Check 'an empty hk.cmd is not ours' (Test-HabakkukForeignHk -Dir $d -Shim $shim)
    foreach ($ext in 'exe', 'bat', 'com') {
        $d = New-TestDir $ext
        Set-Content -LiteralPath (Join-Path $d "hk.$ext") -Value 'x' -Encoding ASCII
        Check "hk.$ext is not ours" (Test-HabakkukForeignHk -Dir $d -Shim $shim)
    }
} finally {
    Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
}

if ($failed) {
    Write-Host "$failed failed"
    exit 1
}
Write-Host 'all passed'
