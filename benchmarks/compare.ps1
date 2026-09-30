param(
    [Parameter(Mandatory)][string]$BeforeBinary,
    [Parameter(Mandatory)][string]$AfterBinary,
    [ValidateRange(1, 100)][int]$Repetitions = 7,
    [string]$OutputDirectory = (Join-Path $PSScriptRoot 'results')
)

$ErrorActionPreference = 'Stop'
$binaries = @{
    before = (Resolve-Path -LiteralPath $BeforeBinary).Path
    after = (Resolve-Path -LiteralPath $AfterBinary).Path
}
$null = New-Item -ItemType Directory -Force -Path $OutputDirectory
$outputs = @{}
foreach ($label in @('before', 'after')) {
    $outputs[$label] = Join-Path $OutputDirectory "optimization-$label.txt"
    Set-Content -LiteralPath $outputs[$label] -Value '' -Encoding utf8
}
$previousGOGC = $env:GOGC
$previousLimit = $env:GOMEMLIMIT
$previousProcs = $env:GOMAXPROCS
try {
    $env:GOGC = '100'
    $env:GOMEMLIMIT = 'off'
    $env:GOMAXPROCS = '8'
    for ($round = 0; $round -lt $Repetitions; $round++) {
        # Alternate run order to reduce systematic before/after timing bias.
        $order = if ($round % 2 -eq 0) { @('before', 'after') } else { @('after', 'before') }
        foreach ($label in $order) {
            & $binaries[$label] '-test.run=^$' '-test.bench=^BenchmarkLifecycle$' '-test.benchtime=1x' '-test.count=1' '-test.cpu=8' '-test.benchmem' |
                Add-Content -LiteralPath $outputs[$label] -Encoding utf8
            if ($LASTEXITCODE -ne 0) { throw "$label benchmark failed: $LASTEXITCODE" }
        }
        Write-Output "Completed comparison pair $($round + 1)/$Repetitions"
    }
} finally {
    $env:GOGC = $previousGOGC
    $env:GOMEMLIMIT = $previousLimit
    $env:GOMAXPROCS = $previousProcs
}
