export const initialClientState = {
  connection: {
    status: "idle",
    reconnectAttempt: 0,
    lastError: "",
  },
  session: {
    clientSessionId: "",
    dbSessionId: "",
    state: "idle",
    phase: "",
    mode: "",
    recovered: false,
  },
  baseline: null,
  latestAnalysis: null,
  report: null,
  transcript: [],
  audio: {
    muted: false,
    queue: [],
    microphoneStatus: "idle",
    microphoneError: "",
    playbackStatus: "idle",
    playbackError: "",
  },
  ui: {
    error: "",
    logs: [],
  },
};

export function createSessionId() {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `speech-coach-ui-${Math.random().toString(36).slice(2, 10)}`;
}

export function reduceEvent(state, event) {
  const next = structuredClone(state);
  switch (event.type) {
    case "session.state":
      next.session.dbSessionId = event.db_session_id || next.session.dbSessionId;
      next.session.state = event.state || next.session.state;
      next.session.phase = event.phase || next.session.phase;
      next.session.mode = event.mode || next.session.mode;
      next.session.recovered = Boolean(event.recovered) || next.session.recovered;
      if (event.state === "ready") {
        next.ui.error = "";
      }
      return next;
    case "session.recovered":
      next.session.dbSessionId = event.db_session_id || next.session.dbSessionId;
      next.session.phase = event.phase || next.session.phase;
      next.session.state = "ready";
      next.session.recovered = true;
      next.baseline = event.baseline || next.baseline;
      next.latestAnalysis = event.latest_analysis || next.latestAnalysis;
      next.transcript = Array.isArray(event.transcript)
        ? event.transcript.map((entry, index) => ({
          id: `${entry.turn_id || entry.speaker || "recovered"}-${index}`,
          speaker: entry.speaker || "system",
          text: entry.text || "",
        }))
        : next.transcript;
      next.transcript.push({
        id: `recovered-${next.transcript.length}`,
        speaker: "system",
        text: "Recovered the previous browser session.",
      });
      next.ui.error = "";
      return next;
    case "user.transcript.provisional":
      next.transcript = next.transcript.filter((entry) => entry.id !== `${event.turn_id || "user"}-user`);
      next.transcript.push({
        id: `${event.turn_id || "user"}-user-provisional`,
        speaker: "user",
        text: `${event.text || ""} (checking)`,
      });
      return next;
    case "user.transcript.final":
      next.transcript = next.transcript.filter((entry) => entry.id !== `${event.turn_id || "user"}-user-provisional`);
      next.transcript.push({
        id: `${event.turn_id || "user"}-user`,
        speaker: "user",
        text: event.text || "",
      });
      return next;
    case "assistant.text.final":
      next.transcript.push({
        id: `${event.turn_id || "assistant"}-assistant`,
        speaker: "assistant",
        text: event.text || "",
      });
      return next;
    case "assistant.audio.chunk":
      if (event.audio_b64) {
        next.audio.queue.push({
          turnId: event.turn_id || "",
          chunkIndex: event.chunk_index ?? 0,
          audio_b64: event.audio_b64,
          sampleRateHz: event.sample_rate_hz || 16000,
          encoding: event.encoding || "pcm_s16le",
          isFinal: Boolean(event.is_final),
        });
      }
      return next;
    case "session.report":
      next.report = {
        sessionId: event.db_session_id || next.session.dbSessionId,
        reason: event.reason || "",
        startedAt: event.started_at || "",
        endedAt: event.ended_at || "",
        duration: event.duration || "",
        turns: event.turns || 0,
        averagePitchHz: event.average_pitch_hz || 0,
        trend: event.trend || "",
        previousSessionAvgHz: event.previous_session_avg_hz || 0,
        baseline: event.baseline || null,
        bestTurn: event.best_turn || null,
        recentTurns: Array.isArray(event.recent_turns) ? event.recent_turns : [],
      };
      next.transcript.push({
        id: `report-${next.transcript.length}`,
        speaker: "system",
        text: `Session ended with ${event.turns || 0} completed turn(s).`,
      });
      return next;
    case "baseline.ready":
      next.baseline = event.baseline || null;
      next.transcript.push({
        id: `baseline-${next.transcript.length}`,
        speaker: "system",
        text: event.source === "passive" ? "Baseline learned from the conversation." : "Baseline captured.",
      });
      return next;
    case "turn.analysis":
      next.latestAnalysis = event.analysis || null;
      return next;
    case "session.error":
      next.ui.error = event.message || "Session error.";
      return next;
    default:
      return next;
  }
}

export function persistSnapshot(state, sessionId) {
  void state;
  void sessionId;
  return JSON.stringify({
    audio: {
      muted: state.audio.muted,
    },
  });
}

export function hydrateSnapshot(raw) {
  if (!raw) {
    return initialClientState;
  }
  try {
    const parsed = JSON.parse(raw);
    return {
      ...structuredClone(initialClientState),
      audio: {
        ...structuredClone(initialClientState).audio,
        muted: Boolean(parsed.audio?.muted),
      },
    };
  } catch {
    return initialClientState;
  }
}
