class SpeechCoachMicCaptureProcessor extends AudioWorkletProcessor {
  process(inputs, outputs) {
    const input = inputs[0]?.[0];
    if (input && input.length > 0) {
      this.port.postMessage(input.slice(0));
    }

    const output = outputs[0]?.[0];
    if (output) {
      output.fill(0);
    }
    return true;
  }
}

registerProcessor("speech-coach-mic-capture", SpeechCoachMicCaptureProcessor);
