export const VERTEX_SOURCE = `#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`;

export const FRAGMENT_SOURCE = `#version 300 es
precision mediump float;

in vec2 vUv;
uniform float uTime;
uniform vec2 uResolution;
uniform float uQuality;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoGlow;
uniform float uProgress;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(123.34, 456.21));
  p += dot(p, p + 45.32);
  return fract(p.x * p.y);
}

float valueNoise(vec2 p) {
  vec2 cell = floor(p);
  vec2 local = fract(p);
  local = local * local * (3.0 - 2.0 * local);
  float a = hash21(cell);
  float b = hash21(cell + vec2(1.0, 0.0));
  float c = hash21(cell + vec2(0.0, 1.0));
  float d = hash21(cell + vec2(1.0, 1.0));
  return mix(mix(a, b, local.x), mix(c, d, local.x), local.y);
}

void main() {
  vec2 aspectUv = vUv;
  aspectUv.x *= uResolution.x / max(uResolution.y, 1.0);
  float cells = mix(14.0, 72.0, uDensity) * uScale * mix(0.6, 1.0, uQuality);
  float grain = valueNoise(aspectUv * cells + vec2(uTime * uSpeed * 0.04, 0.0));
  float threshold = mix(-0.08, 1.08, uProgress);
  float feather = mix(0.11, 0.035, uDensity);
  float revealed = smoothstep(threshold - feather, threshold + feather, grain);
  float edge = 1.0 - smoothstep(0.0, feather * 2.5, abs(grain - threshold));
  float vignette = smoothstep(1.05, 0.18, length(vUv - 0.5));
  vec3 color = mix(uAtmoNear, uTint, 0.72) + uAtmoGlow * edge * 0.18;
  float alpha = (revealed * 0.22 + edge * 0.78) * vignette * uIntensity;
  fragColor = vec4(color, alpha);
}
`;
