param(
    [string]$OnnxRoot = "C:\tools\onnxruntime-win-x64-1.18.1",
    [string]$Output = "voice-coach.exe",
    [switch]$UseSilero
)

$ErrorActionPreference = "Stop"

if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) {
    throw "gcc not found on PATH. Add your MSYS2 UCRT64 bin directory to PATH first."
}

if (-not (Get-Command pkg-config -ErrorAction SilentlyContinue) -and -not (Get-Command pkgconf -ErrorAction SilentlyContinue)) {
    throw "pkg-config/pkgconf not found on PATH. Add your MSYS2 UCRT64 bin directory to PATH first."
}

$env:CGO_ENABLED = "1"

$tags = "portaudio"
if ($UseSilero) {
    $includeDir = Join-Path $OnnxRoot "include"
    $libDir = Join-Path $OnnxRoot "lib"
    $binDir = Join-Path $OnnxRoot "bin"
    $dllDir = if (Test-Path (Join-Path $binDir "onnxruntime.dll")) { $binDir } else { $libDir }

    $requiredPaths = @(
        $includeDir,
        $libDir,
        (Join-Path $includeDir "onnxruntime_c_api.h"),
        (Join-Path $dllDir "onnxruntime.dll")
    )

    foreach ($path in $requiredPaths) {
        if (-not (Test-Path $path)) {
            throw "Required ONNX Runtime path not found: $path"
        }
    }

    $env:CGO_CFLAGS = "-I$includeDir"
    $env:CGO_LDFLAGS = "-L$libDir -lonnxruntime"
    $env:PATH = "$dllDir;$env:PATH"
    $tags = "silero portaudio"
}

go build -tags $tags -o $Output .
