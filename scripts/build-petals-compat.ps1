param(
    [string]$PetalsSource = './tmp/petals-source',
    [string]$HivemindSource = './tmp/hivemind-source',
    [string]$MultiaddrSource = './tmp/multiaddr-source',
    [string]$DaemonBinary = './tmp/p2pd-linux-amd64',
    [string]$Tag = 'compute-platform-petals-compat:local'
)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$buildArgs = @('build', '--platform', 'linux/amd64', '--progress=plain')
$sources = @(
    @{Name='petals'; Path=$PetalsSource; Revision='22afba627a7eb4fcfe9418c49472c6a51334b8ac'},
    @{Name='hivemind'; Path=$HivemindSource; Revision='213bff98a62accb91f254e2afdccbf1d69ebdea9'},
    @{Name='multiaddr'; Path=$MultiaddrSource; Revision='e01dbd38f2c0464c0f78b556691d655265018cce'}
)
foreach ($source in $sources) {
    $sourcePath = (Resolve-Path -LiteralPath $source.Path).Path
    $head = git -C $sourcePath rev-parse HEAD
    if ($LASTEXITCODE -ne 0 -or $head -ne $source.Revision) { throw "Wrong source revision: $($source.Name)" }
    $dirty = git --no-optional-locks -C $sourcePath status --porcelain
    if ($LASTEXITCODE -ne 0 -or $dirty) { throw "Source checkout must be clean: $($source.Name)" }
    $buildArgs += @('--build-context', "$($source.Name)-source=$sourcePath")
}
$binaryPath = (Resolve-Path -LiteralPath $DaemonBinary).Path
$hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $binaryPath).Hash.ToLowerInvariant()
if ($hash -ne '42f8f48e62583b97cdba3c31439c08029fb2b9fc506b5bdd82c46b7cc1d279d8') { throw 'Wrong p2pd binary checksum' }
$cache = Join-Path $projectRoot 'tmp/petals-p2pd-cache'
New-Item -ItemType Directory -Force $cache | Out-Null
Copy-Item -LiteralPath $binaryPath -Destination (Join-Path $cache 'p2pd-linux-amd64') -Force
$buildArgs += @('--build-context', "p2pd-cache=$cache", '-t', $Tag, '-f', (Join-Path $projectRoot 'deploy/compose/Dockerfile.petals-compat'), $projectRoot)
& docker @buildArgs
if ($LASTEXITCODE -ne 0) { throw 'Petals compatibility image build failed' }
