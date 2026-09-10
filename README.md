# Speech Coach

Speech Coach is a standalone Go application for speech practice. It combines
real-time transcription, conversational coaching, text-to-speech, and pitch
feedback on top of the reusable
[`real-time-voice-pipeline-go`](https://github.com/etimbukafia/real-time-voice-pipeline-go)
module.

## Modes

| Mode | Purpose | Audio devices |
| --- | --- | --- |
| `analyze` | Record a sample and report vocal range. This is the default. | Local microphone and PortAudio |
| `coach` | Run an interactive coaching session with pitch feedback. | Local microphone, speaker, and PortAudio |
| `browser` | Run the coach through the browser. | Browser microphone and playback |

Browser mode is the recommended path on Windows because it does not require
local PortAudio devices. Coach sessions are stored in SQLite; use `--db` to
choose a different database path.

## Quick start: browser mode

The application depends on a private, versioned Go module. Make sure your
GitHub account can read the shared pipeline repository, then download the
dependencies:

```powershell
$env:GOPRIVATE = 'github.com/etimbukafia/*'
go mod download
```

Create a local environment file and set the provider credentials:

```powershell
Copy-Item .env.example .env
```

At minimum, browser mode needs:

```dotenv
VOICE_COACH_STT_PROVIDER=python-mistral
VOICE_COACH_STT_PYTHON_EXE=C:\path\to\venv\Scripts\python.exe
MISTRAL_API_KEY=...
MISTRAL_BASE_URL=https://api.mistral.ai
CARTESIA_API_KEY=...
CARTESIA_VOICE_ID=...
```

Build the React client and the Go server:

```powershell
Push-Location .\frontend
npm.cmd ci
npm.cmd run build
Pop-Location

go build -o speech-coach.exe .
```

Start the server:

```powershell
.\speech-coach.exe --mode=browser --listen=127.0.0.1:8080
```

Open <http://127.0.0.1:8080> and select `Connect`, then `Start Mic`.

The built frontend is embedded in the Go binary from `web/`. React source and
development tooling live in `frontend/`.

## Speech providers

The default STT provider is `python-mistral`. Install its Python dependency in
the interpreter configured by `VOICE_COACH_STT_PYTHON_EXE`:

```powershell
C:\path\to\venv\Scripts\python.exe -m pip install "mistralai[realtime]>=2.4.0"
```

You can also set `VOICE_COACH_STT_PROVIDER` to `assemblyai` or `go-websocket`.
The provider-specific variables and optional tuning settings are documented in
[`.env.example`](.env.example).

## Local audio modes

Local microphone and speaker modes require the native PortAudio toolchain:

```powershell
.\build.ps1
.\speech-coach.exe --mode=analyze
.\speech-coach.exe --mode=coach
```

Use `.\build.ps1 -UseSilero` to build with Silero VAD. The script expects
`gcc`, `pkg-config` (or `pkgconf`), and the ONNX Runtime installation specified
by its `-OnnxRoot` parameter.

## Frontend development

Run the Vite development server from `frontend/`:

```powershell
Push-Location .\frontend
npm.cmd ci
npm.cmd run dev
Pop-Location
```

## Validation

```powershell
go test ./...
go vet ./...
go build ./...
```
