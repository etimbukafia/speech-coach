import "./app.css";
import { useSpeechCoachSession } from "./useSpeechCoachSession.js";

function connectionLabel(status) {
  if (status === "connected") {
    return "online";
  }
  if (status === "connecting") {
    return "connecting";
  }
  if (status === "disconnected") {
    return "reconnecting";
  }
  return "offline";
}

function phaseCopy(session, microphoneStatus, playbackStatus) {
  if (microphoneStatus === "recording") {
    return "Listening now";
  }
  if (session.state === "processing") {
    return "Thinking";
  }
  if (session.state === "speaking" || playbackStatus === "playing") {
    return "Coach speaking";
  }
  if (session.phase === "awaiting_baseline") {
    return "Learning your baseline";
  }
  return "Ready";
}

function SessionBadge({ connection, phase }) {
  return (
    <div className="session-badge">
      <span className={`status-dot status-${connection}`} />
      <span>{connectionLabel(connection)}</span>
      <span className="mode-pill">{phase || "starting"}</span>
    </div>
  );
}

function StatusCard({ state }) {
  const message = state.audio.microphoneError || state.audio.playbackError || state.connection.lastError || state.ui.error;
  const livePhase = phaseCopy(state.session, state.audio.microphoneStatus, state.audio.playbackStatus);
  const isBusy = livePhase === "Listening now" || livePhase === "Thinking" || livePhase === "Coach speaking";

  return (
    <article className="status-card">
      <p className="eyebrow">Session</p>
      <h3>{state.connection.status === "connected" ? livePhase : "Waiting to connect"}</h3>
      <p className={`activity-pill ${isBusy ? "is-active" : ""}`}>{livePhase}</p>
      <p className="muted-copy">
        {state.audio.microphoneStatus === "recording"
          ? "Listening now. Speak naturally, then pause at the end of your thought."
          : "Connect, then start your mic when you are ready."}
      </p>
      {state.session.recovered ? <p className="status-note">Session restored.</p> : null}
      {state.connection.reconnectAttempt > 0 ? <p className="status-note">Reconnecting...</p> : null}
      {message ? <p className="status-error">{message}</p> : null}
    </article>
  );
}

function BaselineCard({ baseline }) {
  return (
    <article className="insight-card">
      <p className="eyebrow">Baseline</p>
      {baseline ? (
        <>
          <h3>{Math.round(baseline.median_freq_hz)} Hz</h3>
          <p>{baseline.median_note} - {baseline.voice_type}</p>
          <p>{baseline.low_note} to {baseline.high_note}</p>
        </>
      ) : (
        <p className="muted-copy">A natural baseline is learned from early conversation turns.</p>
      )}
    </article>
  );
}

function AnalysisCard({ analysis }) {
  return (
    <article className="insight-card">
      <p className="eyebrow">Latest Turn</p>
      {analysis ? (
        <>
          <h3>{analysis.median_freq_hz ? `${Math.round(analysis.median_freq_hz)} Hz` : "No pitch"}</h3>
          <p>{analysis.median_note || "?"} - {analysis.voice_type || "unknown"}</p>
          <p>{analysis.pitch_report || "No pitch report returned."}</p>
        </>
      ) : (
        <p className="muted-copy">Pitch feedback appears after each completed turn.</p>
      )}
    </article>
  );
}

function ReportCard({ report }) {
  return (
    <article className="insight-card">
      <p className="eyebrow">Progress</p>
      {report ? (
        <>
          <h3>{report.turns} turn{report.turns === 1 ? "" : "s"}</h3>
          <p>{report.duration || "Session duration unavailable."}</p>
          <p>
            {report.averagePitchHz ? `${Math.round(report.averagePitchHz)} Hz avg` : "No average pitch yet"}
            {report.trend ? ` - ${report.trend}` : ""}
          </p>
        </>
      ) : (
        <p className="muted-copy">Your session summary appears after you end the session.</p>
      )}
    </article>
  );
}

function AudioControls({ state, api }) {
  const isTurnBusy = state.session.state === "processing" || state.session.state === "speaking";
  const canStartMic = state.connection.status === "connected"
    && state.audio.microphoneStatus !== "recording"
    && state.audio.microphoneStatus !== "requesting"
    && !isTurnBusy;
  const canStopMic = state.audio.microphoneStatus === "recording";
  const connectLabel = state.connection.reconnectAttempt > 0 || state.session.recovered ? "Reconnect" : "Connect";

  return (
    <div className="control-cluster">
      <button type="button" onClick={() => api.connect()} disabled={state.connection.status === "connected" || state.connection.status === "connecting"}>
        {connectLabel}
      </button>
      <button type="button" className="secondary" onClick={() => api.disconnect()} disabled={state.connection.status === "idle"}>
        Disconnect
      </button>
      <button type="button" onClick={() => void api.startMicrophone()} disabled={!canStartMic}>
        Start Mic
      </button>
      <button type="button" className="secondary" onClick={() => void api.stopMicrophone()} disabled={!canStopMic}>
        Stop & Send
      </button>
      <button type="button" className="secondary" onClick={() => api.toggleMute()}>
        {state.audio.muted ? "Unmute" : "Mute"}
      </button>
    </div>
  );
}

function Timeline({ transcript }) {
  return (
    <div className="timeline">
      {transcript.length === 0 ? (
        <article className="timeline-entry timeline-system">
          <strong>Session</strong>
          <p>Start speaking when you are ready.</p>
        </article>
      ) : transcript.map((entry) => (
        <article key={entry.id} className={`timeline-entry timeline-${entry.speaker}`}>
          <strong>{entry.speaker === "assistant" ? "Coach" : entry.speaker === "user" ? "You" : "System"}</strong>
          <p>{entry.text}</p>
        </article>
      ))}
    </div>
  );
}

export default function App() {
  const websocketUrl = import.meta.env.VITE_SPEECH_COACH_WS_URL
    || `${window.location.protocol === "https:" ? "wss" : "ws"}://${window.location.host}/ws`;
  const { state, api } = useSpeechCoachSession({ websocketUrl });

  return (
    <main className="app-shell">
      <section className="hero-panel">
        <div>
          <p className="eyebrow">Speech Coach</p>
          <h1>Build a deeper, steadier voice through real conversation.</h1>
          <p className="hero-copy">Short turns. Clear feedback. Measurable progress.</p>
        </div>
        <SessionBadge
          connection={state.connection.status}
          phase={state.session.phase || state.session.mode}
        />
      </section>

      <AudioControls state={state} api={api} />

      <section className="dashboard-grid">
        <StatusCard state={state} />
        <BaselineCard baseline={state.baseline} />
        <AnalysisCard analysis={state.latestAnalysis} />
        <ReportCard report={state.report} />
      </section>

      <section className="conversation-shell">
        <div className="conversation-header">
          <div>
            <p className="eyebrow">Conversation</p>
            <h2>Live Coaching</h2>
          </div>
        </div>
        <Timeline transcript={state.transcript} />
      </section>
    </main>
  );
}
