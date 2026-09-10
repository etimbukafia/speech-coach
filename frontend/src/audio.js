export const TARGET_SAMPLE_RATE = 16000;
export const FRAME_DURATION_MS = 20;
export const FRAME_SAMPLES = (TARGET_SAMPLE_RATE * FRAME_DURATION_MS) / 1000;

export function downsampleToPcm16(input, inputSampleRate, outputSampleRate = TARGET_SAMPLE_RATE) {
  if (!(input instanceof Float32Array)) {
    throw new TypeError("downsampleToPcm16 expects Float32Array input");
  }
  if (inputSampleRate === outputSampleRate) {
    return float32ToPcm16(input);
  }
  if (inputSampleRate <= 0 || outputSampleRate <= 0 || inputSampleRate < outputSampleRate) {
    throw new RangeError("downsampleToPcm16 requires inputSampleRate >= outputSampleRate > 0");
  }

  const ratio = inputSampleRate / outputSampleRate;
  const outputLength = Math.max(1, Math.floor(input.length / ratio));
  const result = new Int16Array(outputLength);
  let inputOffset = 0;

  for (let outputOffset = 0; outputOffset < outputLength; outputOffset += 1) {
    const nextInputOffset = Math.min(input.length, Math.round((outputOffset + 1) * ratio));
    let total = 0;
    let count = 0;
    for (let index = inputOffset; index < nextInputOffset; index += 1) {
      total += input[index];
      count += 1;
    }
    const sample = count > 0 ? total / count : input[inputOffset] ?? 0;
    result[outputOffset] = floatToInt16(sample);
    inputOffset = nextInputOffset;
  }
  return result;
}

export function appendPcm16(left, right) {
  const merged = new Int16Array(left.length + right.length);
  merged.set(left);
  merged.set(right, left.length);
  return merged;
}

export function splitPcm16Frames(samples, frameSamples = FRAME_SAMPLES) {
  const frames = [];
  let offset = 0;
  while (samples.length - offset >= frameSamples) {
    frames.push(samples.slice(offset, offset + frameSamples));
    offset += frameSamples;
  }
  return {
    frames,
    remainder: samples.slice(offset),
  };
}

export function pcm16ToBase64(samples) {
  const bytes = new Uint8Array(samples.length * 2);
  const view = new DataView(bytes.buffer);
  for (let index = 0; index < samples.length; index += 1) {
    view.setInt16(index * 2, samples[index], true);
  }
  return bytesToBase64(bytes);
}

export function base64ToPcm16(base64) {
  const bytes = base64ToBytes(base64);
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const samples = new Int16Array(bytes.byteLength / 2);
  for (let index = 0; index < samples.length; index += 1) {
    samples[index] = view.getInt16(index * 2, true);
  }
  return samples;
}

export function pcm16ToFloat32(samples) {
  const floats = new Float32Array(samples.length);
  for (let index = 0; index < samples.length; index += 1) {
    floats[index] = samples[index] / 32768;
  }
  return floats;
}

export function pcm16RmsNormalized(samples) {
  if (!(samples instanceof Int16Array) || samples.length === 0) {
    return 0;
  }
  let energy = 0;
  for (let index = 0; index < samples.length; index += 1) {
    const normalized = samples[index] / 32768;
    energy += normalized * normalized;
  }
  return Math.sqrt(energy / samples.length);
}

function float32ToPcm16(input) {
  const samples = new Int16Array(input.length);
  for (let index = 0; index < input.length; index += 1) {
    samples[index] = floatToInt16(input[index]);
  }
  return samples;
}

function floatToInt16(sample) {
  const clamped = Math.max(-1, Math.min(1, sample));
  return clamped < 0 ? Math.round(clamped * 32768) : Math.round(clamped * 32767);
}

function bytesToBase64(bytes) {
  let binary = "";
  for (let index = 0; index < bytes.length; index += 1) {
    binary += String.fromCharCode(bytes[index]);
  }
  return btoa(binary);
}

function base64ToBytes(base64) {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes;
}
