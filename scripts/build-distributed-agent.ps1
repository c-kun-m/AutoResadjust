param(
    [string]$SourceDirectory,
    [ValidatePattern('^[0-9a-f]{40}$')][string]$Revision = 'e6ab7c1a41054a888ada952eab4c886444c2f5ad',
    [string]$CudaArchitectures = '75;80;86;89;120',
    [string]$Tag = 'compute-platform-distributed-agent:local'
)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$dockerfile = Join-Path $projectRoot 'deploy/compose/Dockerfile.distributed-agent'
$buildArgs = @('build', '--progress=plain', '--build-arg', "LLAMA_CPP_REF=$Revision", '--build-arg', "CUDA_ARCHITECTURES=$CudaArchitectures", '-t', $Tag)
if ($SourceDirectory) {
    $sourcePath = (Resolve-Path -LiteralPath $SourceDirectory).Path
    $actualRevision = git -C $sourcePath rev-parse HEAD
    if ($LASTEXITCODE -ne 0 -or $actualRevision -ne $Revision) { throw 'Local source must be checked out at the requested llama.cpp revision.' }
    $changes = git --no-optional-locks -C $sourcePath status --porcelain
    if ($LASTEXITCODE -ne 0 -or $changes) { throw 'Use an unmodified llama.cpp checkout for reproducible engine validation.' }
    $content = [IO.File]::ReadAllText($dockerfile)
    $fetchLine = 'RUN git init && git remote add origin https://github.com/ggml-org/llama.cpp.git && git fetch --depth 1 origin ${LLAMA_CPP_REF} && git checkout FETCH_HEAD'
    if (-not $content.Contains($fetchLine)) { throw 'Engine Dockerfile changed; update the local-source adapter before building.' }
    $scratch = Join-Path $projectRoot 'tmp'
    New-Item -ItemType Directory -Force $scratch | Out-Null
    $dockerfile = Join-Path $scratch 'Dockerfile.distributed-agent.local'
    [IO.File]::WriteAllText($dockerfile, $content.Replace($fetchLine, 'COPY --from=llama-source / /engine/'))
    $buildArgs += @('--build-context', "llama-source=$sourcePath")
}
$buildArgs += @('-f', $dockerfile, $projectRoot)
& docker @buildArgs
if ($LASTEXITCODE -ne 0) { throw 'Distributed agent image build failed.' }
