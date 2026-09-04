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
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;
uniform float uProgress;
uniform vec2 uDirection;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
uniform float uSoftness;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(127.1, 311.7));
  return fract(sin(dot(p, vec2(41.3, 17.7))) * 4375.85);
}

void main() {
  vec2 direction = normalize(uDirection + vec2(0.0001));
  vec2 centred = vUv - 0.5;
  float directional = dot(centred, direction) + 0.5;
  float field = mix(dot(mix(uAtmoNear, uAtmoFar, vUv.y), vec3(0.22, 0.7, 0.08)), directional, 0.64);
  float cells = mix(5.0, 28.0, uDensity) * uScale * mix(0.6, 1.0, uQuality);
  float grain = (hash21(floor(vUv * cells + vec2(uTime * uSpeed * 0.03))) - 0.5) * 0.14;
  float luminance = clamp(field + grain, 0.0, 1.0);
  float threshold = uProgress;
  float wipe = smoothstep(threshold - uSoftness, threshold + uSoftness, luminance);
  float edge = 1.0 - smoothstep(0.0, uSoftness * 2.8, abs(luminance - threshold));
  vec3 color = mix(uAtmoFar, uTint, 0.76) + uAtmoGlow * edge * 0.2;
  float alpha = (1.0 - wipe) * 0.26 * uIntensity + edge * 0.74 * uIntensity;
  fragColor = vec4(color, alpha * (0.55 + 0.45 * uQuality));
}
`;
