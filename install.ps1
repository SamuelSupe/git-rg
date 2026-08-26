#requires -Version 5.1

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$Version = 'latest',

    [string]$InstallDir = '',

    [switch]$NoPathUpdate
)

$ErrorActionPreference = 'Stop'

$Repository = 'SamuelSupe/git-rg'
$RepositoryUrl = "https://github.com/$Repository"
$ApiUrl = "https://api.github.com/repos/$Repository/releases/latest"
$TempDir = $null
$StagingPath = $null

function Throw-InstallerError {
    param([string]$Message)

    throw "git-rg installer: $Message"
}

function Normalize-Version {
    param([string]$InputVersion)

    if ($InputVersion -ieq 'latest') {
        try {
            $release = Invoke-RestMethod -Uri $ApiUrl -Headers @{
                Accept = 'application/vnd.github+json'
                'User-Agent' = 'git-rg-installer'
            } -UseBasicParsing -ErrorAction Stop
            $InputVersion = [string]$release.tag_name
        }
        catch {
            Throw-InstallerError 'could not resolve the latest GitHub release'
        }
    }

    if ($InputVersion -cnotmatch '^v') {
        $InputVersion = "v$InputVersion"
    }

    if ($InputVersion -cnotmatch '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$') {
        Throw-InstallerError "invalid release version: $InputVersion (expected v0.2.0 or 0.2.0)"
    }

    return $InputVersion
}

function Download-ReleaseFile {
    param(
        [string]$Uri,
        [string]$Destination,
        [string]$Description
    )

    try {
        Invoke-WebRequest -Uri $Uri -OutFile $Destination -UseBasicParsing -ErrorAction Stop
    }
    catch {
        Throw-InstallerError "could not download $Description"
    }
}

function Test-PathEntry {
    param(
        [AllowNull()]
        [string]$PathValue,
        [Parameter(Mandatory = $true)]
        [string]$Entry
    )

    if ([string]::IsNullOrWhiteSpace($PathValue)) {
        return $false
    }

    $normalizedEntry = $Entry.Trim().TrimEnd([char[]]('/\'))
    foreach ($candidate in ($PathValue -split ';')) {
        if ($candidate.Trim().TrimEnd([char[]]('/\')) -ieq $normalizedEntry) {
            return $true
        }
    }
    return $false
}

function Get-ReleaseChecksum {
    param(
        [string]$ChecksumPath,
        [string]$AssetName
    )

    $matchCount = 0
    $valid = $true
    $checksum = $null
    foreach ($line in (Get-Content -LiteralPath $ChecksumPath -ErrorAction Stop)) {
        if ($line -match '^(?<hash>\S+)\s+(?<name>\S+)(?<rest>.*)$' -and $Matches['name'] -ceq $AssetName) {
            $matchCount++
            $checksum = [string]$Matches['hash']
            if ($Matches['rest'] -ne '' -or $checksum -notmatch '^[0-9A-Fa-f]{64}$') {
                $valid = $false
            }
        }
    }

    if ($matchCount -ne 1 -or -not $valid) {
        Throw-InstallerError "checksums.txt does not contain one valid entry for $AssetName"
    }

    return $checksum
}

function Copy-VerifiedZipEntry {
    param(
        [Parameter(Mandatory = $true)]
        $Entry,
        [Parameter(Mandatory = $true)]
        [string]$Destination
    )

    $entryStream = $null
    $fileStream = $null
    try {
        $entryStream = $Entry.Open()
        $fileStream = [System.IO.File]::Open(
            $Destination,
            [System.IO.FileMode]::CreateNew,
            [System.IO.FileAccess]::Write,
            [System.IO.FileShare]::None
        )
        $entryStream.CopyTo($fileStream)
    }
    catch {
        Throw-InstallerError 'could not extract the verified git-rg binary'
    }
    finally {
        if ($null -ne $fileStream) {
            $fileStream.Dispose()
        }
        if ($null -ne $entryStream) {
            $entryStream.Dispose()
        }
    }
}

try {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        Throw-InstallerError 'unsupported operating system; this script supports Windows only'
    }

    if ([string]::IsNullOrWhiteSpace($InstallDir)) {
        if ([string]::IsNullOrWhiteSpace($env:LOCALAPPDATA)) {
            Throw-InstallerError 'LOCALAPPDATA is not set; pass -InstallDir explicitly'
        }
        $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\git-rg\bin'
    }

    $architectureName = $env:PROCESSOR_ARCHITEW6432
    if ([string]::IsNullOrWhiteSpace($architectureName)) {
        $architectureName = $env:PROCESSOR_ARCHITECTURE
    }
    if ([string]::IsNullOrWhiteSpace($architectureName)) {
        Throw-InstallerError 'could not determine the Windows processor architecture'
    }
    switch ($architectureName.ToUpperInvariant()) {
        'AMD64' { $ReleaseArchitecture = 'amd64' }
        'ARM64' { $ReleaseArchitecture = 'arm64' }
        default {
            Throw-InstallerError "unsupported architecture: $architectureName; prebuilt releases support x64 and arm64"
        }
    }

    $ResolvedVersion = Normalize-Version $Version
    $AssetName = "git-rg_${ResolvedVersion}_windows_${ReleaseArchitecture}.zip"
    $ArchiveRoot = [System.IO.Path]::GetFileNameWithoutExtension($AssetName)
    $ExpectedBinary = "$ArchiveRoot/git-rg.exe"

    $TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("git-rg-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

    $BaseUrl = "$RepositoryUrl/releases/download/$ResolvedVersion"
    $ChecksumPath = Join-Path $TempDir 'checksums.txt'
    $ArchivePath = Join-Path $TempDir $AssetName
    Download-ReleaseFile "$BaseUrl/checksums.txt" $ChecksumPath 'checksums.txt'
    Download-ReleaseFile "$BaseUrl/$AssetName" $ArchivePath $AssetName

    $ExpectedChecksum = Get-ReleaseChecksum $ChecksumPath $AssetName
    try {
        $ActualChecksum = (Get-FileHash -Algorithm SHA256 -LiteralPath $ArchivePath -ErrorAction Stop).Hash
    }
    catch {
        Throw-InstallerError "could not calculate the checksum for $AssetName"
    }
    if ($ActualChecksum -ine $ExpectedChecksum) {
        Throw-InstallerError "checksum verification failed for $AssetName"
    }

    try {
        Add-Type -AssemblyName System.IO.Compression.FileSystem -ErrorAction Stop
        $zip = [System.IO.Compression.ZipFile]::OpenRead($ArchivePath)
    }
    catch {
        Throw-InstallerError "could not open verified archive $AssetName"
    }

    $binaryPath = Join-Path $TempDir 'git-rg.exe'
    try {
        $entries = @($zip.Entries)
        if ($entries.Count -lt 1 -or $entries.Count -gt 2) {
            Throw-InstallerError "archive $AssetName contains unexpected files"
        }

        $binaryEntry = $null
        $rootEntryFound = $false
        foreach ($entry in $entries) {
            $entryName = [string]$entry.FullName
            if ($entryName -ceq "$ArchiveRoot/") {
                if ($rootEntryFound) {
                    Throw-InstallerError "archive $AssetName contains duplicate directory entries"
                }
                if ($entry.Length -ne 0) {
                    Throw-InstallerError "archive $AssetName contains an invalid directory entry"
                }
                $rootEntryFound = $true
            }
            elseif ($entryName -ceq $ExpectedBinary) {
                if ($null -ne $binaryEntry -or $entryName.EndsWith('/')) {
                    Throw-InstallerError "archive $AssetName contains an invalid binary entry"
                }
                if ($entry.Length -le 0) {
                    Throw-InstallerError "archive $AssetName contains an empty binary"
                }
                $binaryEntry = $entry
            }
            else {
                Throw-InstallerError "archive $AssetName contains unexpected files or links"
            }
        }

        if ($null -eq $binaryEntry) {
            Throw-InstallerError "archive $AssetName is missing its expected binary"
        }
        Copy-VerifiedZipEntry $binaryEntry $binaryPath
    }
    finally {
        $zip.Dispose()
    }

    $binaryItem = Get-Item -LiteralPath $binaryPath -Force -ErrorAction Stop
    if ($binaryItem.PSIsContainer -or (($binaryItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)) {
        Throw-InstallerError 'extracted git-rg is not a regular file'
    }
    try {
        $ReportedVersion = & $binaryPath --version
        if ($LASTEXITCODE -ne 0 -or $ReportedVersion -ne "git-rg $ResolvedVersion") {
            Throw-InstallerError "verified archive contains unexpected version: $ReportedVersion"
        }
    }
    catch {
        Throw-InstallerError 'verified archive contains a binary that cannot run on this platform'
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $targetPath = Join-Path $InstallDir 'git-rg.exe'
    if (Test-Path -LiteralPath $targetPath -PathType Container) {
        Throw-InstallerError "install target is a directory: $targetPath"
    }
    $StagingPath = Join-Path $InstallDir ('.git-rg-' + [Guid]::NewGuid().ToString('N') + '.tmp')
    Copy-Item -LiteralPath $binaryPath -Destination $StagingPath -Force -ErrorAction Stop
    Move-Item -LiteralPath $StagingPath -Destination $targetPath -Force -ErrorAction Stop
    $StagingPath = $null

    if ($NoPathUpdate) {
        Write-Output ("Installed git-rg {0} to {1}. PATH was not changed." -f $ResolvedVersion, $targetPath)
    }
    else {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        if (-not (Test-PathEntry $userPath $InstallDir)) {
            if ([string]::IsNullOrWhiteSpace($userPath)) {
                $newUserPath = $InstallDir
            }
            else {
                $newUserPath = "$userPath;$InstallDir"
            }
            try {
                [Environment]::SetEnvironmentVariable('Path', $newUserPath, 'User')
            }
            catch {
                Throw-InstallerError 'could not update the user PATH; use -NoPathUpdate and add the directory manually'
            }
        }

        if (-not (Test-PathEntry $env:Path $InstallDir)) {
            if ([string]::IsNullOrWhiteSpace($env:Path)) {
                $env:Path = $InstallDir
            }
            else {
                $env:Path = "$InstallDir;$env:Path"
            }
        }
        Write-Output ("Installed git-rg {0} to {1} and updated the user PATH." -f $ResolvedVersion, $targetPath)
    }
}
catch {
    [Console]::Error.WriteLine($_.Exception.Message)
    exit 1
}
finally {
    if ($null -ne $StagingPath -and (Test-Path -LiteralPath $StagingPath)) {
        Remove-Item -LiteralPath $StagingPath -Force -ErrorAction SilentlyContinue
    }
    if ($null -ne $TempDir -and (Test-Path -LiteralPath $TempDir)) {
        Remove-Item -LiteralPath $TempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
