// uElementRect is supplied from the target DOMRect as normalized
// [left, bottom, width, height] coordinates in the effect canvas.
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
uniform vec2 uResolution;
uniform float uPixelRatio;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uDensity;
uniform float uGrainSize;
uniform float uSpeed;
uniform float uCornerRadius;
uniform float uGrainDetail;
uniform float uAlphaCap;

out vec4 fragColor;

float hash12(vec2 point) {
  return fract(sin(dot(point, vec2(127.1, 311.7))) * 437.58);
}

float roundedBox(vec2 point, vec2 halfSize, float radius) {
  vec2 q = abs(point) - halfSize + radius;
  return length(max(q, 0.0)) + min(max(q.x, q.y), 0.0) - radius;
}

void main() {
  float aspect = uElementRect.z / max(uElementRect.w, 0.0001);
  vec2 point = vUv - 0.5;
  point.x *= aspect;
  float radius = min(uCornerRadius, min(0.49, 0.49 * aspect));
  float shape = 1.0 - smoothstep(0.0, 0.012, roundedBox(point, vec2(0.5 * aspect, 0.5), radius));

  // Work in physical pixels. Multiplying the CSS-sized grain cell by DPR
  // keeps the grain crisp and at a stable apparent size on dense displays.
  vec2 physicalSize = max(uElementRect.zw * uResolution, vec2(1.0));
  vec2 pixel = vUv * physicalSize;
  float dpr = max(uPixelRatio, 1.0);
  float cell = max(0.5, uGrainSize * dpr);
  vec2 lattice = floor(pixel / cell * max(uDensity, 0.1));
  float coarse = hash12(lattice + floor(uTime * uSpeed * 5.0));
  float fine = hash12(floor(pixel / dpr));
  float grain = mix(coarse, fine, uGrainDetail);
  vec3 base = mix(uAtmoNear, uAtmoFar, vUv.y);
  vec3 color = mix(base, uTint, 0.2 + grain * 0.5);
  float alpha = shape * uIntensity * uAlphaCap * (0.72 + grain * 0.28);
  fragColor = vec4(color, alpha);
}
`;
