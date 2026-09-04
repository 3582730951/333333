// Rendered over one liquid-looking control/card (or a group sharing one
// union rect); uElementRect is a normalized DOMRect supplied by the caller.
export const VERTEX_SOURCE = `#version 300 es
precision mediump float;

uniform vec4 uElementRect;
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  vec2 position = uElementRect.xy + corner * uElementRect.zw;
  gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}
`;

export const FRAGMENT_SOURCE = `#version 300 es
precision mediump float;

in vec2 vUv;
uniform float uTime;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoGlow;
uniform vec3 uAtmoFar;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uWaveScale;
uniform float uThickness;
uniform float uSoftness;
uniform float uOffset;
uniform float uWaveDetail;
uniform float uAlphaCap;

out vec4 fragColor;

float roundedBox(vec2 point, vec2 halfSize, float radius) {
  vec2 q = abs(point) - halfSize + radius;
  return length(max(q, 0.0)) + min(max(q.x, q.y), 0.0) - radius;
}

void main() {
  float aspect = uElementRect.z / max(uElementRect.w, 0.0001);
  vec2 point = vUv - 0.5;
  point.x *= aspect;
  float shape = 1.0 - smoothstep(0.0, 0.012, roundedBox(point, vec2(0.5 * aspect, 0.5), 0.1));
  float phase = uTime * uSpeed;
  float wave = sin(vUv.x * uWaveScale + phase) * 0.055 * uWaveDetail;
  wave += sin(vUv.x * uWaveScale * 0.47 - phase * 0.72) * 0.035 * uWaveDetail;
  float centre = clamp(0.75 + uOffset + wave, 0.12, 0.94);
  float distanceToRidge = abs(vUv.y - centre);
  float ridge = 1.0 - smoothstep(uThickness, uThickness + uSoftness, distanceToRidge);
  float secondary = 1.0 - smoothstep(uThickness * 0.65, uThickness + uSoftness * 1.7, abs(vUv.y - centre - 0.065));
  float topMask = smoothstep(0.28, 0.72, vUv.y);
  float flow = 0.5 + 0.5 * sin(vUv.x * 7.0 - phase * 0.5);
  vec3 color = mix(uAtmoNear, uAtmoGlow, flow * 0.5 + ridge * 0.5);
  color = mix(color, uTint, 0.38 + ridge * 0.4);
  float alpha = shape * topMask * uIntensity * uAlphaCap * (ridge * 0.9 + secondary * 0.3);
  fragColor = vec4(color, alpha);
}
`;
