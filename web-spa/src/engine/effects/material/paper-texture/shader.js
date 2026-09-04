// A paper surface is one DOM element or a group with a common union rect;
// callers derive uElementRect from getBoundingClientRect() at layout changes.
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
uniform vec2 uResolution;
uniform float uPixelRatio;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;
uniform float uTime;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uFiberScale;
uniform float uFiberStrength;
uniform float uAge;
uniform float uCornerRadius;
uniform float uDetail;
uniform float uAlphaCap;

out vec4 fragColor;

float hash12(vec2 point) {
  return fract(sin(dot(point, vec2(23.17, 91.43))) * 317.29);
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
  vec2 physical = max(uElementRect.zw * uResolution, vec2(1.0));
  vec2 pixel = vUv * physical;
  float dpr = max(uPixelRatio, 1.0);
  float fiberCell = max(1.0, uFiberScale * dpr);
  float broad = hash12(floor(pixel / fiberCell));
  float fine = hash12(floor(pixel / dpr));
  float threads = 0.5 + 0.5 * sin(pixel.y / fiberCell * 2.8 + pixel.x / fiberCell * 0.32 + uTime * 0.08);
  float texture = mix(broad, fine, uDetail) * 0.72 + threads * 0.28;
  vec3 paper = mix(uAtmoNear, uAtmoFar, 0.28 + vUv.y * 0.44);
  paper = mix(paper, uTint, 0.45 + uAge * 0.18);
  paper += (texture - 0.5) * uFiberStrength;
  float alpha = shape * uIntensity * uAlphaCap * (0.7 + texture * 0.3);
  fragColor = vec4(max(paper, vec3(0.0)), alpha);
}
`;
