import { useEffect, useReducer, useRef } from "react";

import {
  FRAME_DURATION_MS,
  FRAME_SAMPLES,
  TARGET_SAMPLE_RATE,
  appendPcm16,
  base64ToPcm16,
  downsampleToPcm16,
  pcm16RmsNormalized,
  pcm16ToBase64,
  pcm16ToFloat32,
  splitPcm16Frames,
} from "./audio.js";
import { createSessionId, hydrateSnapshot, initialClientState, persistSnapshot, reduceEvent } from "./state.js";

const WORKLET_URL = new URL("./mic-capture-worklet.js", import.meta.url);
const BASE_SPEECH_RMS_THRESHOLD = 0.018;
const MIN_SPEECH_RMS_MARGIN = 0.008;
const BARGE_IN_SPEECH_RMS_THRESHOLD = 0.03;
const DEFAULT_SILENCE_MS = 780;
const SHORT_TURN_SILENCE_MS = 980;
const LONG_TURN_SILENCE_MS = 620;
const NOISE_FLOOR_ALPHA = 0.15;
const PREROLL_FRAME_LIMIT = 12;
const STORAGE_KEY = "speech-coach-ui-snapshot";
const CONNECT_TIMEOUT_MS = 10000;
const DISCONNECT_REPORT_TIMEOUT_MS = 2500;
const MIN_HOT_FRAMES_TO_START = 3;
const BARGE_IN_HOT_FRAMES = 5;
const MIN_TURN_FRAMES = 8;

function reducer(state, action) {
  switch (action.type) {
    case "hydrate":
      return {
        ...hydrateSnapshot(action.payload),
        session: {
          ...hydrateSnapshot(action.payload).session,
          clientSessionId: action.sessionId,
        },
      };
    case "server_event":
      return reduceEvent(state, action.payload);
    case "connecting":
      return {
        ...state,
        connection: {
          ...state.connection,
          status: "connecting",
          reconnectAttempt: action.reconnectAttempt,
        },
        report: null,
      };
    case "connected":
      return {
        ...state,
        connection: {
          status: "connected",
          reconnectAttempt: 0,
          lastError: "",
        },
        report: null,
      };
    case "disconnected":
      return {
        ...state,
        connection: {
          ...state.connection,
          status: "disconnected",
          lastError: action.error || "",
        },
      };
    case "microphone_state":
      return {
        ...state,
        audio: {
          ...state.audio,
          microphoneStatus: action.status,
          microphoneError: action.error || "",
        },
      };
    case "playback_state":
      return {
        ...state,
        audio: {
          ...state.audio,
          playbackStatus: action.status,
          playbackError: action.error || "",
        },
      };
    case "queued_audio":
      return {
        ...state,
        audio: {
          ...state.audio,
          queue: Array.from({ length: action.count }, (_, index) => state.audio.queue[index] ?? { chunkIndex: index }),
        },
      };
    case "toggle_mute":
      return {
        ...state,
        audio: {
          ...state.audio,
          muted: !state.audio.muted,
        },
      };
    case "clear_queue":
      return {
        ...state,
        audio: {
          ...state.audio,
          queue: [],
        },
      };
    case "local_error":
      return {
        ...state,
        ui: {
          ...state.ui,
          error: action.message,
        },
      };
    case "local_log":
      return {
        ...state,
        ui: {
          ...state.ui,
          logs: [...state.ui.logs, action.entry].slice(-150),
        },
      };
    default:
      return state;
  }
}

function readStoredSnapshot() {
  if (typeof window === "undefined") {
    return "";
  }
  return window.sessionStorage.getItem(STORAGE_KEY) ?? "";
}

function getAudioContextCtor() {
  if (typeof window === "undefined") {
    return null;
  }
  return window.AudioContext || window.webkitAudioContext || null;
}

export function useSpeechCoachSession({ websocketUrl }) {
  const storedSnapshotRef = useRef(readStoredSnapshot());
  const initialSnapshot = hydrateSnapshot(storedSnapshotRef.current);
  const sessionIdRef = useRef(initialSnapshot.session.clientSessionId || createSessionId());
  const [state, dispatch] = useReducer(reducer, {
    ...initialClientState,
    session: {
      ...initialClientState.session,
      clientSessionId: sessionIdRef.current,
    },
    audio: {
      ...initialClientState.audio,
      muted: initialSnapshot.audio.muted,
    },
  });

  const wsRef = useRef(null);
  const connectPromiseRef = useRef(null);
  const reconnectAttemptRef = useRef(0);
  const shouldReconnectRef = useRef(false);
  const reconnectTimerRef = useRef(null);
  const connectTimeoutRef = useRef(null);
  const mutedRef = useRef(initialSnapshot.audio.muted);
  const connectionStatusRef = useRef(initialClientState.connection.status);
  const sessionStateRef = useRef(initialClientState.session.state);
  const sessionPhaseRef = useRef(initialClientState.session.phase);
  const microphoneStatusRef = useRef(initialClientState.audio.microphoneStatus);
  const queuedAudioRef = useRef([]);
  const pendingDisconnectRef = useRef(false);
  const disconnectTimerRef = useRef(null);
  const shouldAutoResumeMicRef = useRef(false);
  const pendingMicResumeRef = useRef(false);
  const assistantAudibleRef = useRef(false);
  const playbackStatusRef = useRef(initialClientState.audio.playbackStatus);

  const captureStreamRef = useRef(null);
  const captureContextRef = useRef(null);
  const captureSourceRef = useRef(null);
  const captureWorkletNodeRef = useRef(null);
  const captureSinkRef = useRef(null);
  const captureRemainderRef = useRef(new Int16Array(0));
  const frameSequenceRef = useRef(0);
  const captureSpeechDetectedRef = useRef(false);
  const captureSpeechActiveRef = useRef(false);
  const captureTurnSettlingRef = useRef(false);
  const capturePrerollFramesRef = useRef([]);
  const captureSilenceTimerRef = useRef(null);
  const captureHotFrameCountRef = useRef(0);
  const captureActiveFrameCountRef = useRef(0);
  const captureNoiseFloorRef = useRef(0.004);

  const playbackContextRef = useRef(null);
  const playbackCursorRef = useRef(0);
  const playbackSourcesRef = useRef(new Set());

  function clearConnectTimeout() {
    if (connectTimeoutRef.current) {
      window.clearTimeout(connectTimeoutRef.current);
      connectTimeoutRef.current = null;
    }
  }

  function clearDisconnectTimer() {
    if (disconnectTimerRef.current) {
      window.clearTimeout(disconnectTimerRef.current);
      disconnectTimerRef.current = null;
    }
  }

  useEffect(() => {
    mutedRef.current = state.audio.muted;
  }, [state.audio.muted]);

  useEffect(() => {
    connectionStatusRef.current = state.connection.status;
    sessionStateRef.current = state.session.state;
    sessionPhaseRef.current = state.session.phase;
    microphoneStatusRef.current = state.audio.microphoneStatus;
  }, [state.connection.status, state.session.state, state.session.phase, state.audio.microphoneStatus]);

  useEffect(() => {
    playbackStatusRef.current = state.audio.playbackStatus;
  }, [state.audio.playbackStatus]);

  function logEvent(level, message, details = "") {
    const entry = {
      id: `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
      at: new Date().toISOString(),
      level,
      message,
      details,
    };
    dispatch({ type: "local_log", entry });
    const logger = level === "error" ? console.error : level === "warn" ? console.warn : console.info;
    logger(`[speech-coach] ${message}`, details || "");
  }

  useEffect(() => {
    dispatch({ type: "hydrate", payload: storedSnapshotRef.current, sessionId: sessionIdRef.current });
  }, []);

  useEffect(() => {
    if (typeof window === "undefined") {
      return undefined;
    }
    const snapshot = persistSnapshot(state, sessionIdRef.current);
    window.sessionStorage.setItem(STORAGE_KEY, snapshot);
    return undefined;
  }, [state]);

  useEffect(() => {
    return () => {
      void stopMicrophone({ notifyServer: false, stopTracks: true });
      stopPlayback();
      const playbackContext = playbackContextRef.current;
      playbackContextRef.current = null;
      if (playbackContext) {
        void playbackContext.close().catch(() => {});
      }
      disconnect();
    };
  }, []);

  useEffect(() => {
    void pumpPlaybackQueue();
  }, [state.audio.muted, state.audio.queue.length]);

  async function ensureConnected() {
    if (!websocketUrl) {
      dispatch({ type: "local_error", message: "Speech coach websocket URL is not configured." });
      logEvent("error", "WebSocket URL is not configured");
      return false;
    }
    const current = wsRef.current;
    if (current && current.readyState === WebSocket.OPEN) {
      return true;
    }
    if (connectPromiseRef.current) {
      return connectPromiseRef.current;
    }

    shouldReconnectRef.current = true;
    dispatch({ type: "connecting", reconnectAttempt: reconnectAttemptRef.current });
    logEvent("info", "Opening websocket connection", websocketUrl);

    connectPromiseRef.current = new Promise((resolve, reject) => {
      const nextSocket = new WebSocket(websocketUrl);
      let settled = false;
      wsRef.current = nextSocket;

      const settle = (callback, value) => {
        if (settled) {
          return;
        }
        settled = true;
        connectPromiseRef.current = null;
        callback(value);
      };

      connectTimeoutRef.current = window.setTimeout(() => {
        if (settled) {
          return;
        }
        const message = `Timed out after ${CONNECT_TIMEOUT_MS / 1000}s waiting for websocket open`;
        dispatch({ type: "disconnected", error: message });
        dispatch({ type: "local_error", message });
        logEvent("error", message, websocketUrl);
        try {
          nextSocket.close();
        } catch {}
      }, CONNECT_TIMEOUT_MS);

      nextSocket.addEventListener("open", () => {
        clearConnectTimeout();
        const shouldResumeSession = reconnectAttemptRef.current > 0;
        reconnectAttemptRef.current = 0;
        dispatch({ type: "connected" });
        logEvent("info", "WebSocket connected");
        sendPayload(nextSocket, {
          type: "client.session.start",
          session_id: sessionIdRef.current,
          resume: shouldResumeSession,
          input_mode: "audio",
          sample_rate_hz: TARGET_SAMPLE_RATE,
          audio_codec: "pcm_s16le",
        });
        logEvent("info", "Sent client.session.start", `${sessionIdRef.current} resume=${shouldResumeSession}`);
        void ensurePlaybackContext();
        settle(resolve, true);
      });

      nextSocket.addEventListener("message", (message) => {
        const payload = JSON.parse(message.data);
        if (payload?.type === "assistant.audio.chunk" && payload.audio_b64) {
          assistantAudibleRef.current = true;
          queuedAudioRef.current.push(payload);
          dispatch({ type: "queued_audio", count: queuedAudioRef.current.length });
          if (payload.chunk_index === 0) {
            logEvent("info", "Received assistant audio stream", `${payload.turn_id || "turn"} started`);
          }
          return;
        }
        logEvent("info", `Received server event ${payload?.type || "unknown"}`, summarizePayload(payload));
        if (payload?.type === "session.report" && pendingDisconnectRef.current) {
          pendingDisconnectRef.current = false;
          clearDisconnectTimer();
          logEvent("info", "Received final session report; closing websocket");
          window.setTimeout(() => {
            const socket = wsRef.current;
            if (socket && socket.readyState === WebSocket.OPEN) {
              socket.close(1000, "session ended");
            }
          }, 120);
        }
        if (payload?.type === "session.state") {
          if (payload.state === "ready" || payload.phase === "awaiting_baseline") {
            captureTurnSettlingRef.current = false;
            if (pendingMicResumeRef.current && shouldAutoResumeMicRef.current && microphoneStatusRef.current === "idle") {
              pendingMicResumeRef.current = false;
              logEvent("info", "Auto-resuming microphone after reconnect");
              window.setTimeout(() => {
                void startMicrophone();
              }, 120);
            }
          }
          if (payload.state === "processing" || payload.state === "speaking") {
            endSpeechTurn("server_busy");
          }
        }
        if (payload?.type === "session.recovered" && pendingMicResumeRef.current && shouldAutoResumeMicRef.current && microphoneStatusRef.current === "idle") {
          pendingMicResumeRef.current = false;
          logEvent("info", "Recovered browser session; restarting microphone");
          window.setTimeout(() => {
            void startMicrophone();
          }, 120);
        }
        dispatch({ type: "server_event", payload });
      });

      nextSocket.addEventListener("close", (event) => {
        clearConnectTimeout();
        clearDisconnectTimer();
        pendingDisconnectRef.current = false;
        if (shouldReconnectRef.current && captureWorkletNodeRef.current) {
          pendingMicResumeRef.current = true;
        }
        wsRef.current = null;
        const reason = event.reason || (event.wasClean ? "" : "WebSocket closed.");
        void stopMicrophone({ notifyServer: false, stopTracks: true });
        dispatch({ type: "disconnected", error: reason });
        logEvent(event.wasClean ? "warn" : "error", "WebSocket closed", reason || `code ${event.code}`);
        if (!settled) {
          settle(reject, new Error(reason || "WebSocket closed before connect completed."));
          return;
        }
        if (!shouldReconnectRef.current) {
          return;
        }
        reconnectAttemptRef.current += 1;
        reconnectTimerRef.current = window.setTimeout(() => {
          void ensureConnected();
        }, Math.min(2500, reconnectAttemptRef.current * 500));
      });

      nextSocket.addEventListener("error", () => {
        clearConnectTimeout();
        dispatch({ type: "disconnected", error: "WebSocket error" });
        logEvent("error", "WebSocket emitted error event");
        if (!settled) {
          settle(reject, new Error("WebSocket error"));
        }
      });
    });

    return connectPromiseRef.current;
  }

  function connect() {
    logEvent("info", "Connect requested by user");
    void ensureConnected();
  }

  function disconnect() {
    shouldReconnectRef.current = false;
    clearConnectTimeout();
    clearDisconnectTimer();
    if (reconnectTimerRef.current) {
      window.clearTimeout(reconnectTimerRef.current);
      reconnectTimerRef.current = null;
    }
    logEvent("warn", "Disconnect requested");
    shouldAutoResumeMicRef.current = false;
    pendingMicResumeRef.current = false;
    void stopMicrophone({ notifyServer: false, stopTracks: true });
    stopPlayback();
    const socket = wsRef.current;
    if (!socket) {
      return;
    }
    if (socket.readyState === WebSocket.OPEN) {
      pendingDisconnectRef.current = true;
      sendPayload(socket, {
        type: "client.session.end",
        session_id: sessionIdRef.current,
      });
      logEvent("info", "Sent client.session.end", sessionIdRef.current);
      disconnectTimerRef.current = window.setTimeout(() => {
        pendingDisconnectRef.current = false;
        logEvent("warn", "Session report timeout; forcing websocket close");
        const openSocket = wsRef.current;
        if (openSocket) {
          openSocket.close(1000, "session end timeout");
        }
      }, DISCONNECT_REPORT_TIMEOUT_MS);
      return;
    }
    wsRef.current = null;
    socket.close();
  }

  function sendPayload(socket, payload) {
    socket.send(JSON.stringify(payload));
  }

  function send(payload) {
    const socket = wsRef.current;
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      logEvent("warn", `Dropped outbound event ${payload?.type || "unknown"}`, "socket not open");
      return false;
    }
    sendPayload(socket, payload);
    if (payload?.type !== "client.audio.frame") {
      logEvent("info", `Sent client event ${payload?.type || "unknown"}`, summarizePayload(payload));
    }
    return true;
  }

  async function ensurePlaybackContext() {
    const AudioContextCtor = getAudioContextCtor();
    if (!AudioContextCtor) {
      throw new Error("This browser does not support Web Audio.");
    }
    if (!playbackContextRef.current) {
      playbackContextRef.current = new AudioContextCtor();
      logEvent("info", "Created playback AudioContext");
    }
    const context = playbackContextRef.current;
    if (context.state === "suspended") {
      await context.resume();
      logEvent("info", "Resumed playback AudioContext");
    }
    if (playbackCursorRef.current < context.currentTime) {
      playbackCursorRef.current = context.currentTime;
    }
    return context;
  }

  async function primePlayback() {
    try {
      await ensurePlaybackContext();
      return true;
    } catch (error) {
      dispatch({ type: "playback_state", status: "error", error: errorMessage(error, "Audio output is unavailable.") });
      throw error;
    }
  }

  async function pumpPlaybackQueue() {
    if (!queuedAudioRef.current.length) {
      if (state.audio.playbackStatus === "playing" && playbackSourcesRef.current.size === 0) {
        dispatch({ type: "playback_state", status: mutedRef.current ? "muted" : "idle", error: "" });
        assistantAudibleRef.current = false;
      }
      return;
    }
    const chunk = queuedAudioRef.current.shift();
    dispatch({ type: "queued_audio", count: queuedAudioRef.current.length });
    try {
      if (mutedRef.current) {
        dispatch({ type: "playback_state", status: "muted", error: "" });
      } else {
        await primePlayback();
        queueAssistantAudioChunk(chunk);
      }
    } catch (error) {
      dispatch({ type: "playback_state", status: "error", error: errorMessage(error, "Audio playback failed.") });
      stopPlayback();
    }
  }

  function queueAssistantAudioChunk(chunk) {
    if (chunk.encoding !== "pcm_s16le") {
      throw new Error(`Unsupported assistant audio encoding: ${chunk.encoding}`);
    }
    const context = playbackContextRef.current;
    if (!context) {
      throw new Error("Playback context is not ready.");
    }
    const pcm = base64ToPcm16(chunk.audio_b64);
    const floats = pcm16ToFloat32(pcm);
    const buffer = context.createBuffer(1, floats.length, chunk.sample_rate_hz || TARGET_SAMPLE_RATE);
    buffer.copyToChannel(floats, 0);

    const source = context.createBufferSource();
    source.buffer = buffer;
    source.connect(context.destination);

    const startAt = Math.max(context.currentTime, playbackCursorRef.current);
    playbackCursorRef.current = startAt + buffer.duration;
    playbackSourcesRef.current.add(source);
    assistantAudibleRef.current = true;
    source.onended = () => {
      playbackSourcesRef.current.delete(source);
      if (playbackSourcesRef.current.size === 0 && queuedAudioRef.current.length === 0) {
        assistantAudibleRef.current = false;
        dispatch({ type: "playback_state", status: mutedRef.current ? "muted" : "idle", error: "" });
      }
    };
    source.start(startAt);
    dispatch({ type: "playback_state", status: "playing", error: "" });
  }

  function stopPlayback() {
    for (const source of playbackSourcesRef.current) {
      source.onended = null;
      try {
        source.stop();
      } catch {}
      source.disconnect();
    }
    playbackSourcesRef.current.clear();
    assistantAudibleRef.current = false;
    playbackCursorRef.current = 0;
    queuedAudioRef.current = [];
    dispatch({ type: "queued_audio", count: 0 });
    dispatch({ type: "playback_state", status: mutedRef.current ? "muted" : "idle", error: "" });
  }

  function sendAudioFrame(frame) {
    const socket = wsRef.current;
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      return false;
    }
    sendPayload(socket, {
      type: "client.audio.frame",
      session_id: sessionIdRef.current,
      audio_b64: pcm16ToBase64(frame),
      sequence: frameSequenceRef.current,
      frame_duration_ms: FRAME_DURATION_MS,
      sample_rate_hz: TARGET_SAMPLE_RATE,
      encoding: "pcm_s16le",
    });
    frameSequenceRef.current += 1;
    return true;
  }

  function flushCaptureRemainder() {
    const remainder = captureRemainderRef.current;
    if (remainder.length === 0) {
      return;
    }
    const padded = new Int16Array(FRAME_SAMPLES);
    padded.set(remainder.slice(0, FRAME_SAMPLES));
    sendAudioFrame(padded);
    captureRemainderRef.current = new Int16Array(0);
  }

  function sendAudioTurnEnd(reason = "end_of_turn") {
    return send({
      type: "client.audio.end",
      session_id: sessionIdRef.current,
      reason,
    });
  }

  function clearCaptureSilenceTimer() {
    if (captureSilenceTimerRef.current) {
      window.clearTimeout(captureSilenceTimerRef.current);
      captureSilenceTimerRef.current = null;
    }
  }

  function resetSpeechTurnTracking() {
    captureSpeechDetectedRef.current = false;
    captureSpeechActiveRef.current = false;
    capturePrerollFramesRef.current = [];
    captureTurnSettlingRef.current = false;
    captureHotFrameCountRef.current = 0;
    captureActiveFrameCountRef.current = 0;
    captureNoiseFloorRef.current = 0.004;
    clearCaptureSilenceTimer();
  }

  async function stopMicrophone({
    error = "",
    notifyServer = true,
    stopTracks = true,
  } = {}) {
    const workletNode = captureWorkletNodeRef.current;
    const source = captureSourceRef.current;
    const sink = captureSinkRef.current;
    const context = captureContextRef.current;
    const stream = captureStreamRef.current;

    if (!workletNode && !source && !context && !stream) {
      if (error || state.audio.microphoneStatus !== "idle") {
        dispatch({ type: "microphone_state", status: "idle", error });
        if (error) {
          logEvent("warn", "Microphone already idle", error);
        }
      }
      return;
    }

    clearCaptureSilenceTimer();
    flushCaptureRemainder();
    if (notifyServer) {
      sendAudioTurnEnd("end_of_turn");
    }
    logEvent("info", "Stopping microphone capture");

    if (workletNode) {
      workletNode.port.onmessage = null;
      workletNode.disconnect();
    }
    if (source) {
      source.disconnect();
    }
    if (sink) {
      sink.disconnect();
    }
    if (stream && stopTracks) {
      for (const track of stream.getTracks()) {
        track.stop();
      }
    }
    if (context) {
      await context.close().catch(() => {});
    }

    captureWorkletNodeRef.current = null;
    captureSourceRef.current = null;
    captureSinkRef.current = null;
    captureContextRef.current = null;
    captureStreamRef.current = null;
    captureRemainderRef.current = new Int16Array(0);
    resetSpeechTurnTracking();
    dispatch({ type: "microphone_state", status: "idle", error });
    if (error) {
      logEvent("warn", "Microphone stopped with error", error);
    } else {
      logEvent("info", "Microphone stopped");
    }
  }

  function pushPrerollFrame(frame) {
    const next = capturePrerollFramesRef.current;
    next.push(frame);
    if (next.length > PREROLL_FRAME_LIMIT) {
      next.shift();
    }
  }

  function canCaptureTurns() {
    const phase = sessionPhaseRef.current;
    const status = sessionStateRef.current;
    return status === "ready" || status === "listening" || phase === "conversation";
  }

  function isAssistantAudible() {
    return assistantAudibleRef.current || playbackSourcesRef.current.size > 0 || queuedAudioRef.current.length > 0 || playbackStatusRef.current === "playing";
  }

  function currentSpeechThreshold() {
    const adaptive = (captureNoiseFloorRef.current * 2.4) + MIN_SPEECH_RMS_MARGIN;
    return Math.max(BASE_SPEECH_RMS_THRESHOLD, adaptive);
  }

  function currentRequiredHotFrames() {
    if (isAssistantAudible()) {
      return BARGE_IN_HOT_FRAMES;
    }
    return captureNoiseFloorRef.current >= 0.014 ? MIN_HOT_FRAMES_TO_START + 1 : MIN_HOT_FRAMES_TO_START;
  }

  function currentSilenceTimeoutMs() {
    if (captureActiveFrameCountRef.current < 14) {
      return SHORT_TURN_SILENCE_MS;
    }
    if (captureActiveFrameCountRef.current > 42) {
      return LONG_TURN_SILENCE_MS;
    }
    return DEFAULT_SILENCE_MS;
  }

  function updateNoiseFloor(rms) {
    if (captureSpeechActiveRef.current || isAssistantAudible()) {
      return;
    }
    if (rms >= currentSpeechThreshold()) {
      return;
    }
    if (captureNoiseFloorRef.current <= 0) {
      captureNoiseFloorRef.current = rms;
      return;
    }
    captureNoiseFloorRef.current = (captureNoiseFloorRef.current * (1 - NOISE_FLOOR_ALPHA)) + (rms * NOISE_FLOOR_ALPHA);
  }

  function startSpeechTurn() {
    if (captureSpeechActiveRef.current || captureTurnSettlingRef.current || !canCaptureTurns()) {
      return false;
    }
    if (isAssistantAudible()) {
      send({ type: "client.interrupt", session_id: sessionIdRef.current });
      stopPlayback();
      logEvent("warn", "Barge-in interrupt sent", `${captureHotFrameCountRef.current} hot frame(s)`);
    }
    captureSpeechActiveRef.current = true;
    captureActiveFrameCountRef.current = 0;
    logEvent("info", "Detected speech start", `threshold ${currentSpeechThreshold().toFixed(3)}`);
    for (const frame of capturePrerollFramesRef.current) {
      sendAudioFrame(frame);
      captureActiveFrameCountRef.current += 1;
    }
    capturePrerollFramesRef.current = [];
    return true;
  }

  function endSpeechTurn(reason = "end_of_turn") {
    if (!captureSpeechActiveRef.current) {
      return;
    }
    if (captureActiveFrameCountRef.current < MIN_TURN_FRAMES) {
      send({ type: "client.interrupt", session_id: sessionIdRef.current });
      logEvent("warn", "Canceled short false-start turn", `${captureActiveFrameCountRef.current} frame(s)`);
      captureSpeechActiveRef.current = false;
      captureSpeechDetectedRef.current = false;
      captureTurnSettlingRef.current = false;
      capturePrerollFramesRef.current = [];
      captureHotFrameCountRef.current = 0;
      captureActiveFrameCountRef.current = 0;
      clearCaptureSilenceTimer();
      return;
    }
    clearCaptureSilenceTimer();
    flushCaptureRemainder();
    sendAudioTurnEnd(reason);
    captureSpeechActiveRef.current = false;
    captureSpeechDetectedRef.current = false;
    captureTurnSettlingRef.current = true;
    capturePrerollFramesRef.current = [];
    logEvent("info", "Ended speech turn", reason);
  }

  function observeCaptureFrame(frame) {
    const rms = pcm16RmsNormalized(frame);
    updateNoiseFloor(rms);
    let emittedCurrentFrame = false;
    if (!captureSpeechActiveRef.current) {
      pushPrerollFrame(frame);
    }
    const threshold = isAssistantAudible()
      ? Math.max(BARGE_IN_SPEECH_RMS_THRESHOLD, currentSpeechThreshold() * 1.35)
      : currentSpeechThreshold();
    if (rms >= threshold) {
      captureHotFrameCountRef.current += 1;
      captureSpeechDetectedRef.current = true;
      clearCaptureSilenceTimer();
      if (captureHotFrameCountRef.current >= currentRequiredHotFrames()) {
        emittedCurrentFrame = startSpeechTurn();
      }
      return emittedCurrentFrame;
    }
    captureHotFrameCountRef.current = 0;
    if (!captureSpeechActiveRef.current || !captureSpeechDetectedRef.current || captureSilenceTimerRef.current) {
      return emittedCurrentFrame;
    }
    captureSilenceTimerRef.current = window.setTimeout(() => {
      captureSilenceTimerRef.current = null;
      if (!captureSpeechActiveRef.current || !captureSpeechDetectedRef.current) {
        return;
      }
      endSpeechTurn("silence_timeout");
    }, currentSilenceTimeoutMs());
    return emittedCurrentFrame;
  }

  async function ensureCaptureWorklet(context) {
    if (!context.audioWorklet) {
      throw new Error("This browser does not support AudioWorklet microphone capture.");
    }
    await context.audioWorklet.addModule(WORKLET_URL);
  }

  function handleCaptureSamples(samples, inputSampleRate) {
    const socket = wsRef.current;
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      return;
    }
    const pcm = downsampleToPcm16(samples, inputSampleRate, TARGET_SAMPLE_RATE);
    const combined = appendPcm16(captureRemainderRef.current, pcm);
    const { frames, remainder } = splitPcm16Frames(combined, FRAME_SAMPLES);
    captureRemainderRef.current = remainder;
    for (const frame of frames) {
      const emittedCurrentFrame = observeCaptureFrame(frame);
      if (captureSpeechActiveRef.current && !emittedCurrentFrame) {
        sendAudioFrame(frame);
        captureActiveFrameCountRef.current += 1;
      }
    }
  }

  async function startMicrophone() {
    if (!globalThis.isSecureContext) {
      dispatch({ type: "microphone_state", status: "error", error: "Microphone capture requires HTTPS or localhost." });
      logEvent("error", "Microphone capture requires HTTPS or localhost");
      return false;
    }
    if (!navigator.mediaDevices?.getUserMedia) {
      dispatch({ type: "microphone_state", status: "error", error: "This browser cannot access the microphone." });
      logEvent("error", "Browser cannot access microphone");
      return false;
    }
    dispatch({ type: "microphone_state", status: "requesting", error: "" });
    logEvent("info", "Microphone start requested");

    try {
      const connected = await ensureConnected();
      if (!connected) {
        return false;
      }
      await primePlayback();
      if (captureWorkletNodeRef.current) {
        return true;
      }

      const AudioContextCtor = getAudioContextCtor();
      if (!AudioContextCtor) {
        throw new Error("This browser does not support Web Audio.");
      }

      const stream = await navigator.mediaDevices.getUserMedia({
        audio: {
          channelCount: 1,
          echoCancellation: true,
          noiseSuppression: true,
          autoGainControl: true,
        },
      });

      const context = new AudioContextCtor({
        latencyHint: "interactive",
        sampleRate: 48000,
      });
      await context.resume();
      await ensureCaptureWorklet(context);
      logEvent("info", "Microphone stream opened");

      const source = context.createMediaStreamSource(stream);
      const workletNode = new AudioWorkletNode(context, "speech-coach-mic-capture", {
        numberOfInputs: 1,
        numberOfOutputs: 1,
        channelCount: 1,
      });
      const sink = context.createGain();
      sink.gain.value = 0;

      workletNode.port.onmessage = (event) => {
        const samples = event.data;
        if (!(samples instanceof Float32Array)) {
          return;
        }
        handleCaptureSamples(samples, context.sampleRate);
      };

      source.connect(workletNode);
      workletNode.connect(sink);
      sink.connect(context.destination);

      captureStreamRef.current = stream;
      captureContextRef.current = context;
      captureSourceRef.current = source;
      captureWorkletNodeRef.current = workletNode;
      captureSinkRef.current = sink;
      captureRemainderRef.current = new Int16Array(0);
      frameSequenceRef.current = 0;
      resetSpeechTurnTracking();
      shouldAutoResumeMicRef.current = true;
      pendingMicResumeRef.current = false;

      dispatch({ type: "microphone_state", status: "recording", error: "" });
      logEvent("info", "Microphone recording started");
      return true;
    } catch (error) {
      await stopMicrophone({
        error: errorMessage(error, "Microphone start failed."),
        notifyServer: false,
        stopTracks: true,
      });
      return false;
    }
  }

  function toggleMute() {
    dispatch({ type: "toggle_mute" });
    if (!mutedRef.current) {
      stopPlayback();
      logEvent("warn", "Muted assistant playback");
    } else {
      void pumpPlaybackQueue();
      logEvent("info", "Unmuted assistant playback");
    }
  }

  function clearQueuedAudio() {
    queuedAudioRef.current = [];
    dispatch({ type: "clear_queue" });
    logEvent("warn", "Cleared queued assistant audio");
  }

  return {
    state,
    api: {
      clearQueuedAudio,
      connect,
      disconnect,
      sessionId: sessionIdRef.current,
      send,
      startMicrophone,
      stopMicrophone: async (...args) => {
        shouldAutoResumeMicRef.current = false;
        pendingMicResumeRef.current = false;
        return stopMicrophone(...args);
      },
      toggleMute,
    },
  };
}

function errorMessage(error, fallback) {
  return error instanceof Error ? error.message : fallback;
}

function summarizePayload(payload) {
  if (!payload || typeof payload !== "object") {
    return "";
  }
  const parts = [];
  if (payload.state) {
    parts.push(`state=${payload.state}`);
  }
  if (payload.phase) {
    parts.push(`phase=${payload.phase}`);
  }
  if (payload.turn_id) {
    parts.push(`turn=${payload.turn_id}`);
  }
  if (payload.reason) {
    parts.push(`reason=${payload.reason}`);
  }
  if (payload.message) {
    parts.push(String(payload.message));
  }
  return parts.join(" ");
}
