# The Repo Depot agent installer for Windows. Run in Terminal (Admin):
#   & ([scriptblock]::Create((irm '__SERVER__/agent/install.ps1'))) -Token 'TOKEN' [-Port 8471] [-AppsDir 'C:\RepoDepot\apps']
# Running it again updates the agent and keeps its settings.
param([string]$Token = "", [int]$Port = 0, [string]$AppsDir = "")
$ErrorActionPreference = "Stop"
$Server = "__SERVER__"

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) { throw "Run this in Terminal (Admin): the agent installs software, so it needs administrator rights." }

$BinDir = Join-Path $env:ProgramFiles "RepoDepot"
$Bin = Join-Path $BinDir "repodepot-agent.exe"
$ConfDir = Join-Path $env:ProgramData "RepoDepot\agent"
$Conf = Join-Path $ConfDir "config.json"

if (-not $Token -and -not (Test-Path $Conf)) { throw "Pass -Token (shown when you add the machine in The Repo Depot)." }

New-Item -ItemType Directory -Force -Path $BinDir, $ConfDir | Out-Null
$old = $null
if (Test-Path $Conf) { $old = Get-Content $Conf -Raw | ConvertFrom-Json }
if (-not $Port) { $Port = if ($old -and $old.listen) { [int]($old.listen.TrimStart(':')) } else { 8471 } }
if (-not $AppsDir) { $AppsDir = if ($old -and $old.apps_dir) { $old.apps_dir } else { "C:\RepoDepot\apps" } }
if (-not $Token) { $Token = $old.token }

$svc = Get-Service RepoDepotAgent -ErrorAction SilentlyContinue
if ($svc) { Stop-Service RepoDepotAgent -Force -ErrorAction SilentlyContinue; Start-Sleep 2 }

Write-Host "Downloading the agent from $Server"
$tmp = "$Bin.new"
Invoke-WebRequest -UseBasicParsing "$Server/agent/download/windows-amd64" -OutFile $tmp
Move-Item -Force $tmp $Bin

@{ token = $Token; listen = ":$Port"; apps_dir = $AppsDir } | ConvertTo-Json | Set-Content -Encoding UTF8 $Conf
icacls $ConfDir /inheritance:r /grant:r "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F" | Out-Null
New-Item -ItemType Directory -Force -Path $AppsDir | Out-Null

if (-not (Get-NetFirewallRule -DisplayName "The Repo Depot Agent" -ErrorAction SilentlyContinue)) {
  New-NetFirewallRule -DisplayName "The Repo Depot Agent" -Direction Inbound -Protocol TCP -LocalPort $Port -Action Allow -Profile Private,Domain | Out-Null
}

& $Bin -config $Conf install-service
Start-Sleep 2
$v = & $Bin version
Write-Host "The Repo Depot agent $v is running on port $Port."
