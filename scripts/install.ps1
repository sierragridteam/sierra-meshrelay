# Installs meshbridge on Windows: downloads the release for this machine,
# verifies its checksum, pairs it with SIERRA and installs the Windows
# service. Run it again to upgrade (an already-paired bridge skips pairing).
# Run it from an elevated PowerShell (Run as administrator), or over SSH as
# an administrator.
#
#   irm https://raw.githubusercontent.com/sierragridteam/sierra-meshrelay/main/scripts/install.ps1 | iex
#
# With options:
#
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/sierragridteam/sierra-meshrelay/main/scripts/install.ps1))) -Passphrase "maple river quartz otter"
#
# Options:
#   -Passphrase WORDS   pair with the passphrase from your SIERRA contact
#                       (otherwise you're asked, or shown a pairing code)
#   -NodeHost ADDR      your node's address, if setup can't find it on the LAN
#   -Port N             your node's TCP port (default 5000)
#   -Version vX.Y.Z     install that release instead of the latest
#   -NoService          install and pair, but don't install the service
#
# Advanced (testing, private relays):
#   -Relay URL  -Config PATH  -InstallDir DIR  -BaseUrl URL  -NoPath
#
# -InstallDir only matters with -NoService: the service always runs from
# %ProgramFiles%\SIERRA\meshbridge.
#
# Everything is inside Install-MeshBridge, called on the last line, so a
# download cut short runs nothing. It never calls exit, which would close the
# window of someone running it with `irm | iex`.

param(
	[string]$Passphrase = "",
	[string]$NodeHost = "",
	[int]$Port = 0,
	[string]$Version = "latest",
	[switch]$NoService,
	[string]$Relay = "",
	[string]$Config = "",
	[string]$InstallDir = "",
	[string]$BaseUrl = "https://github.com/sierragridteam/sierra-meshrelay/releases",
	[switch]$NoPath
)

function Install-MeshBridge {
	param($Passphrase, $NodeHost, $Port, $Version, $NoService, $Relay, $Config, $InstallDir, $BaseUrl, $NoPath)
	$ErrorActionPreference = "Stop"
	$ProgressPreference = "SilentlyContinue" # Invoke-WebRequest is very slow with a progress bar
	if (-not $Config) { $Config = Join-Path $env:ProgramData "SIERRA\meshbridge\meshbridge.toml" }
	if (-not $InstallDir) { $InstallDir = Join-Path $env:ProgramFiles "SIERRA\meshbridge" }

	$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
		[Security.Principal.WindowsBuiltInRole]::Administrator)
	if (-not $admin) {
		throw "Run this from an elevated PowerShell (right-click PowerShell, Run as administrator)."
	}

	$cpu = $env:PROCESSOR_ARCHITEW6432
	if (-not $cpu) { $cpu = $env:PROCESSOR_ARCHITECTURE }
	switch ($cpu) {
		"AMD64" { $arch = "amd64" }
		"ARM64" { $arch = "arm64" }
		default { throw "unsupported CPU: $cpu" }
	}
	$asset = "meshbridge_windows_$arch.zip"
	if ($Version -eq "latest") { $url = "$BaseUrl/latest/download" } else { $url = "$BaseUrl/download/$Version" }

	# Windows PowerShell 5.1 may not offer TLS 1.2 by default; GitHub needs it.
	[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

	$tmp = Join-Path ([IO.Path]::GetTempPath()) ("meshbridge-" + [guid]::NewGuid().ToString("N"))
	New-Item -ItemType Directory -Path $tmp | Out-Null
	try {
		Write-Host "Downloading meshbridge ($Version, windows/$arch)..."
		Invoke-WebRequest -UseBasicParsing -Uri "$url/$asset" -OutFile (Join-Path $tmp $asset)
		Invoke-WebRequest -UseBasicParsing -Uri "$url/SHA256SUMS" -OutFile (Join-Path $tmp "SHA256SUMS")
		$want = $null
		foreach ($line in Get-Content (Join-Path $tmp "SHA256SUMS")) {
			$f = $line -split "\s+"
			if ($f.Count -ge 2 -and $f[1] -eq $asset) { $want = $f[0].ToLower() }
		}
		if (-not $want) { throw "no checksum for $asset in SHA256SUMS" }
		# .NET rather than Get-FileHash and Expand-Archive, which live in
		# modules that may not load (e.g. when started from PowerShell 7).
		$sha = [Security.Cryptography.SHA256]::Create()
		$fs = [IO.File]::OpenRead((Join-Path $tmp $asset))
		try { $got = ([BitConverter]::ToString($sha.ComputeHash($fs)) -replace "-", "").ToLower() } finally { $fs.Dispose() }
		if ($got -ne $want) { throw "checksum mismatch for $asset (got $got, want $want); not installing" }
		Add-Type -AssemblyName System.IO.Compression.FileSystem
		[IO.Compression.ZipFile]::ExtractToDirectory((Join-Path $tmp $asset), (Join-Path $tmp "x"))

		# Windows won't replace a running binary, so stop the service first.
		$svc = Get-Service -Name meshbridge -ErrorAction SilentlyContinue
		$wasRunning = $svc -and $svc.Status -ne "Stopped"
		if ($wasRunning) {
			Write-Host "Stopping the meshbridge service to upgrade it..."
			Stop-Service -Name meshbridge -Force
			(Get-Service -Name meshbridge).WaitForStatus("Stopped", [TimeSpan]::FromSeconds(30))
		}
		New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
		$mb = Join-Path $InstallDir "meshbridge.exe"
		Copy-Item -Force (Join-Path $tmp "x\meshbridge.exe") $mb
		$v = (& $mb version | Select-Object -First 1)
		Write-Host "Installed $v to $mb"
	} finally {
		Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
	}

	if (-not $NoPath) {
		$machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
		if (($machinePath -split ";") -notcontains $InstallDir) {
			[Environment]::SetEnvironmentVariable("Path", ($machinePath.TrimEnd(";") + ";" + $InstallDir), "Machine")
			Write-Host "Added $InstallDir to the system PATH (open a new window to use meshbridge by name)."
		}
		$env:Path = $env:Path + ";" + $InstallDir
	}

	$paired = (Test-Path $Config) -and (Select-String -Quiet -Path $Config -Pattern '^token = "smb_')
	if ($paired) {
		Write-Host "This bridge is already paired ($Config); skipping setup."
	} else {
		Write-Host ""
		$setup = @("setup", "-config", $Config, "-installer")
		if ($Passphrase) { $setup += @("-passphrase", $Passphrase) }
		if ($NodeHost) { $setup += @("-host", $NodeHost) }
		if ($Port -gt 0) { $setup += @("-port", "$Port") }
		if ($Relay) { $setup += @("-relay", $Relay) }
		& $mb @setup
		if ($LASTEXITCODE -ne 0) { throw "setup failed (exit $LASTEXITCODE)" }
	}

	if (-not $NoService) {
		& $mb install -config $Config
		if ($LASTEXITCODE -ne 0) { throw "installing the service failed (exit $LASTEXITCODE)" }
	} else {
		if ($wasRunning) { Start-Service -Name meshbridge }
		Write-Host ""
		Write-Host "Skipped the service. Run the bridge with: & `"$mb`" run -config `"$Config`""
	}
}

Install-MeshBridge -Passphrase $Passphrase -NodeHost $NodeHost -Port $Port -Version $Version -NoService:$NoService `
	-Relay $Relay -Config $Config -InstallDir $InstallDir -BaseUrl $BaseUrl -NoPath:$NoPath
