// Element geometry comes from a DOMRect converted to normalized
// [left, bottom, width, height] effect-canvas coordinates.
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
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uLineSpacing;
uniform float uSpeed;
uniform float uThickness;
uniform float uSoftness;
uniform float uSkew;
uniform float uLineDensity;
uniform float uContrast;
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
  float shape = 1.0 - smoothstep(0.0, 0.012, roundedBox(point, vec2(0.5 * aspect, 0.5), 0.08));
  vec2 physical = max(uElementRect.zw * uResolution, vec2(1.0));
  vec2 pixel = vUv * physical;
  float spacing = max(1.0, uLineSpacing * max(uPixelRatio, 1.0));
  float phase = fract((pixel.y + pixel.x * uSkew) / spacing * uLineDensity + uTime * uSpeed);
  float distanceToLine = abs(phase - 0.5);
  float line = 1.0 - smoothstep(uThickness, uThickness + uSoftness, distanceToLine);
  float shimmer = 0.5 + 0.5 * sin(vUv.x * 9.0 + uTime * uSpeed * 0.7);
  vec3 color = mix(uAtmoFar, uAtmoGlow, line * (0.55 + shimmer * 0.35));
  color = mix(color, uTint, 0.26 + line * 0.3);
  float alpha = shape * uIntensity * uAlphaCap * (0.18 + line * uContrast);
  fragColor = vec4(color, alpha);
}
`;
