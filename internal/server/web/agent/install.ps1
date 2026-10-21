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

# UTF-8 without a byte-order mark (Set-Content -Encoding UTF8 adds one in PowerShell 5.1).
$json = @{ token = $Token; listen = ":$Port"; apps_dir = $AppsDir } | ConvertTo-Json
[IO.File]::WriteAllText($Conf, $json, (New-Object Text.UTF8Encoding $false))
icacls $ConfDir /inheritance:r /grant:r "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F" | Out-Null
New-Item -ItemType Directory -Force -Path $AppsDir | Out-Null

# Any network profile (home networks are often marked Public), but only
# from the local subnet. Re-running fixes the port/profile of an old rule.
$rule = Get-NetFirewallRule -DisplayName "The Repo Depot Agent" -ErrorAction SilentlyContinue
if ($rule) {
  $rule | Set-NetFirewallRule -Profile Any -RemoteAddress LocalSubnet -Enabled True
  $rule | Get-NetFirewallPortFilter | Set-NetFirewallPortFilter -Protocol TCP -LocalPort $Port
} else {
  New-NetFirewallRule -DisplayName "The Repo Depot Agent" -Direction Inbound -Protocol TCP -LocalPort $Port -Action Allow -Profile Any -RemoteAddress LocalSubnet | Out-Null
}

& $Bin -config $Conf install-service
Start-Sleep 2
$v = & $Bin version
if ((Get-Service RepoDepotAgent -ErrorAction SilentlyContinue).Status -ne "Running") {
  throw "The agent service didn't start. Check Event Viewer → Windows Logs → Application."
}
Write-Host "The Repo Depot agent $v is running on port $Port."
