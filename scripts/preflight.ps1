#Requires -Version 5.1
<#
  Expensio local toolchain preflight.
  Checks only. Installs nothing. Prints the winget command for anything missing.

  Exit code: 0 = all required tools present, 1 = at least one required tool is missing.

  Note: messages are kept ASCII on purpose. Windows PowerShell 5.1 reads a
  BOM-less file as ANSI, so non-ASCII literals in this file would be mangled
  on some machines.
#>
[CmdletBinding()]
param()

# Tools installed by winget are not visible to an already-open shell.
# Rebuild PATH from the machine + user scopes before probing.
$env:Path = (@(
        [Environment]::GetEnvironmentVariable('Path', 'Machine'),
        [Environment]::GetEnvironmentVariable('Path', 'User')
    ) -join ';')

$results = New-Object System.Collections.ArrayList

function Add-Result {
    param(
        [string]$Tool,
        [string]$Required,
        [string]$Actual,
        [ValidateSet('OK', 'WARN', 'FAIL')][string]$Status,
        [string]$Hint = ''
    )
    [void]$results.Add([pscustomobject]@{
            Tool     = $Tool
            Required = $Required
            Actual   = $Actual
            Status   = $Status
            Hint     = $Hint
        })
}

function Get-Tool {
    param([string]$Name)
    return (Get-Command $Name -ErrorAction SilentlyContinue)
}

# ---------- required ----------

$go = Get-Tool 'go'
if ($go) {
    $raw = (& go version) 2>$null
    if ($raw -match 'go(\d+\.\d+\.\d+)') {
        $ver = [version]$Matches[1]
        if ($ver -ge [version]'1.23.0') {
            Add-Result 'go' '>= 1.23' $Matches[1] 'OK'
        }
        else {
            Add-Result 'go' '>= 1.23' $Matches[1] 'FAIL' 'winget install --id GoLang.Go --exact'
        }
    }
    else {
        Add-Result 'go' '>= 1.23' 'unparsable' 'FAIL' 'winget install --id GoLang.Go --exact'
    }
}
else {
    Add-Result 'go' '>= 1.23' 'not found' 'FAIL' 'winget install --id GoLang.Go --exact'
}

$make = Get-Tool 'make'
if ($make) {
    $mv = ((& make --version) 2>$null | Select-Object -First 1)
    Add-Result 'make' 'GNU Make 4.x' $mv 'OK'
}
else {
    Add-Result 'make' 'GNU Make 4.x' 'not found' 'FAIL' 'winget install --id ezwinports.make --exact'
}

$bash = $null
if (Test-Path 'C:\Program Files\Git\bin\bash.exe') {
    $bash = 'C:\Program Files\Git\bin\bash.exe'
}
elseif (Get-Tool 'bash') {
    $bash = (Get-Tool 'bash').Source
}
if ($bash) {
    Add-Result 'bash' 'Git for Windows' $bash 'OK'
}
else {
    Add-Result 'bash' 'Git for Windows' 'not found' 'FAIL' 'Makefile needs bash as SHELL'
}

$git = Get-Tool 'git'
if ($git) {
    Add-Result 'git' 'any' ((& git --version) 2>$null) 'OK'
}
else {
    Add-Result 'git' 'any' 'not found' 'FAIL' 'winget install --id Git.Git --exact'
}

$docker = Get-Tool 'docker'
if ($docker) {
    $dv = ((& docker version --format '{{.Client.Version}}') 2>$null)
    Add-Result 'docker (client)' '>= 24' $dv 'OK'

    $server = $null
    try { $server = (& docker info --format '{{.ServerVersion}}') 2>$null } catch { }
    if ($LASTEXITCODE -eq 0 -and $server) {
        Add-Result 'docker (daemon)' 'running' $server 'OK'
    }
    else {
        Add-Result 'docker (daemon)' 'running' 'not reachable' 'FAIL' 'start Docker Desktop'
    }

    $cv = ((& docker compose version) 2>$null)
    if ($cv -match 'v?(\d+\.\d+)') {
        Add-Result 'docker compose' '>= 2.0' $Matches[1] 'OK'
    }
    else {
        Add-Result 'docker compose' '>= 2.0' 'not found' 'FAIL' 'Docker Desktop ships compose v2'
    }
}
else {
    Add-Result 'docker (client)' '>= 24' 'not found' 'FAIL' 'install Docker Desktop'
    Add-Result 'docker (daemon)' 'running' 'not reachable' 'FAIL' 'install Docker Desktop'
    Add-Result 'docker compose' '>= 2.0' 'not found' 'FAIL' 'Docker Desktop ships compose v2'
}

$node = Get-Tool 'node'
if ($node) {
    $nvRaw = (& node --version) 2>$null
    $nv = [version]($nvRaw.TrimStart('v'))
    # Vite 8 requires ^20.19.0 || >=22.12.0
    if ($nv -ge [version]'22.12.0') {
        Add-Result 'node' '>= 22.12' $nvRaw 'OK'
    }
    else {
        Add-Result 'node' '>= 22.12' $nvRaw 'FAIL' 'winget upgrade --id OpenJS.NodeJS.LTS'
    }
}
else {
    Add-Result 'node' '>= 22.12' 'not found' 'FAIL' 'winget install --id OpenJS.NodeJS.LTS --exact'
}

$npm = Get-Tool 'npm'
if ($npm) {
    Add-Result 'npm' 'any' ((& npm --version) 2>$null) 'OK'
}
else {
    Add-Result 'npm' 'any' 'not found' 'FAIL' 'installed with node'
}

# ---------- optional (needed from M6 / M7) ----------

$optional = @{
    'kind'           = 'M6: local Kubernetes cluster'
    'helm'           = 'M6: chart deployment'
    'k6'             = 'M7: load testing'
    'kubectl'        = 'M6: cluster inspection'
    'golangci-lint'  = 'M0: CI lint'
    'sqlc'           = 'M2: query codegen'
    'golang-migrate' = 'M0: schema migrations'
}

foreach ($name in ($optional.Keys | Sort-Object)) {
    if (Get-Tool $name) {
        Add-Result $name 'optional' 'present' 'OK'
    }
    else {
        Add-Result $name 'optional' 'not found' 'WARN' $optional[$name]
    }
}

# ---------- report ----------

$results | Format-Table -AutoSize -Wrap

$fail = @($results | Where-Object { $_.Status -eq 'FAIL' })
$warn = @($results | Where-Object { $_.Status -eq 'WARN' })

""
"required failures: $($fail.Count)"
"optional warnings: $($warn.Count)"

if ($fail.Count -gt 0) {
    ""
    "Install commands for missing required tools:"
    $fail | Where-Object { $_.Hint } | Select-Object -ExpandProperty Hint -Unique | ForEach-Object { "  $_" }
    exit 1
}

exit 0
