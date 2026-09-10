# Speech Coach

`speech-coach` is a standalone Go application built on the reusable
[`real-time-voice-pipeline-go`](https://github.com/etimbukafia/real-time-voice-pipeline-go)
module.

`speech-coach` now supports two runtime shapes:

- `--mode=browser`: recommended on Windows. The browser owns mic capture and playback.
- `--mode=coach` / `--mode=analyze`: local CLI audio modes. These still require a `portaudio` build.

## Browser Mode

Download the Go dependencies before building:

```powershell
$env:GOPRIVATE='github.com/etimbukafia/*'
go mod download
```

Build the React frontend once:

```powershell
cd .\frontend
npm.cmd install
npm.cmd run build
cd ..
```

Then build the Go app without PortAudio:

```powershell
go build -o voice-coach.exe .
```

Run the browser server:

```powershell
.\voice-coach.exe --mode=browser --listen 127.0.0.1:8080
```

Open:

```text
http://127.0.0.1:8080
```

Expected flow:

1. Click `Connect`.
2. The coach speaks the baseline prompt in the browser.
3. Click `Start Mic`.
4. Say the baseline sentence once.
5. Continue speaking turn by turn. The browser auto-detects end-of-turn silence.

Browser mode does not require local PortAudio devices. It uses:

- Python Mistral realtime STT worker, the legacy Go Voxtral websocket client, or AssemblyAI streaming STT
- Mistral LLM
- Cartesia TTS

The built frontend is served from `web/`, and the React source lives in `frontend/`.

## Required Env

Minimum practical browser env:

```dotenv
VOICE_COACH_STT_PROVIDER=python-mistral
VOICE_COACH_STT_PYTHON_EXE=C:\path\to\venv\Scripts\python.exe
MISTRAL_API_KEY=...
MISTRAL_BASE_URL=https://api.mistral.ai
MISTRAL_LLM_MODEL=mistral-small-latest
CARTESIA_API_KEY=...
CARTESIA_VOICE_ID=...
```

AssemblyAI browser env:

```dotenv
VOICE_COACH_STT_PROVIDER=assemblyai
ASSEMBLYAI_API_KEY=...
ASSEMBLYAI_STREAMING_URL=wss://streaming.assemblyai.com/v3/ws
ASSEMBLYAI_SPEECH_MODEL=universal-streaming-english
ASSEMBLYAI_FORMAT_TURNS=true
ASSEMBLYAI_VAD_THRESHOLD=0.4
ASSEMBLYAI_END_OF_TURN_CONFIDENCE_THRESHOLD=0.7
ASSEMBLYAI_MIN_TURN_SILENCE_MS=800
ASSEMBLYAI_MAX_TURN_SILENCE_MS=3600
ASSEMBLYAI_KEYTERMS_PROMPT=Rowan,resonance,grounded
MISTRAL_API_KEY=...
MISTRAL_LLM_MODEL=mistral-small-latest
CARTESIA_API_KEY=...
CARTESIA_VOICE_ID=...
```

The AssemblyAI defaults above are conservative on purpose. They hold the floor longer, which fits reflective coaching turns better than aggressive end-of-turn settings.

Python packages in that interpreter:

```powershell
C:\path\to\venv\Scripts\python.exe -m pip install "mistralai[realtime]>=2.4.0"
```

AssemblyAI keyterm prompting is optional. Start with none, then add only words the recognizer consistently struggles with.

## CLI Audio Modes

If you want `--mode=coach` or `--mode=analyze`, build with PortAudio:

```powershell
go build -tags "portaudio" -o voice-coach.exe .
```

Those modes still depend on local mic/speaker devices and the native PortAudio toolchain.
