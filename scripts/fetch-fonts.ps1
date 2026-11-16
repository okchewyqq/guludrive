# Fetch the MiSans faces that are embedded into the binaries.
#
# The font files are not committed: the MiSans license forbids redistributing
# the font on its own, and the repository only carries the notice and the
# original license text. Run this once after cloning, then build as usual.
#
#   pwsh -File scripts/fetch-fonts.ps1      (Windows)
#   sh   scripts/fetch-fonts.sh             (Linux, macOS)

[CmdletBinding()]
param(
    [string]$Destination = (Join-Path (Split-Path -Parent $PSScriptRoot) 'assets/fonts')
)

$ErrorActionPreference = 'Stop'

# Official release archive of MiSans 4.009, the version NOTICE.txt documents.
$source = 'https://hyperos.mi.com/font-download/MiSans.zip'
$weights = @('Regular', 'Medium', 'Bold')

$fonts = Join-Path $Destination 'fonts.go'
if (Test-Path -LiteralPath $fonts) {
    $present = $weights | Where-Object {
        (Test-Path -LiteralPath (Join-Path $Destination "MiSans-$_.ttf")) -and
        (Test-Path -LiteralPath (Join-Path $Destination "MiSans-$_.woff2"))
    }
    if ($present.Count -eq $weights.Count) {
        Write-Host "MiSans faces already present in $Destination"
        exit 0
    }
}

$work = Join-Path ([IO.Path]::GetTempPath()) "misans-$([Guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Path $work | Out-Null
try {
    $zip = Join-Path $work 'MiSans.zip'
    Write-Host "Downloading $source"
    Invoke-WebRequest -Uri $source -OutFile $zip -UseBasicParsing
    Expand-Archive -LiteralPath $zip -DestinationPath $work

    New-Item -ItemType Directory -Force -Path $Destination | Out-Null
    foreach ($weight in $weights) {
        foreach ($format in @('ttf', 'woff2')) {
            $match = Get-ChildItem -LiteralPath $work -Recurse -File -Filter "MiSans-$weight.$format" |
                Select-Object -First 1
            if (-not $match) {
                throw "MiSans-$weight.$format is missing from $source"
            }
            Copy-Item -LiteralPath $match.FullName -Destination (Join-Path $Destination $match.Name) -Force
            Write-Host "  $($match.Name)"
        }
    }
    Write-Host "MiSans faces copied to $Destination"
} finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
