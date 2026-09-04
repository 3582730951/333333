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
uniform vec3 uAtmoGlow;
uniform float uProgress;
uniform vec4 uFromRect;
uniform vec4 uToRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
uniform float uCornerRadius;
uniform float uGeometryReady;
out vec4 fragColor;

float roundedBox(vec2 point, vec2 halfSize, float radius) {
  vec2 q = abs(point) - halfSize + radius;
  return length(max(q, vec2(0.0))) + min(max(q.x, q.y), 0.0) - radius;
}

void main() {
  vec4 rect = mix(uFromRect, uToRect, uProgress);
  vec2 centre = rect.xy + rect.zw * 0.5;
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  vec2 point = (vUv - centre) * aspect;
  vec2 halfSize = max(rect.zw * 0.5 * aspect, vec2(0.001));
  float radius = min(min(halfSize.x, halfSize.y) * uCornerRadius * 2.0, 0.45);
  float distanceToBox = roundedBox(point, halfSize, radius);
  float lineWidth = mix(0.003, 0.012, uDensity) * uScale;
  float outline = 1.0 - smoothstep(lineWidth, lineWidth * 2.4, abs(distanceToBox));
  float fill = 1.0 - smoothstep(0.0, lineWidth * 2.0, distanceToBox);
  float shimmer = 0.82 + 0.18 * sin((point.x + point.y) * mix(20.0, 64.0, uDensity) - uTime * uSpeed * 3.0);
  vec3 colour = mix(uTint, uAtmoGlow, 0.22);
  float alpha = (outline * 0.8 + fill * 0.08) * shimmer * uIntensity * uGeometryReady * (0.58 + 0.42 * uQuality);
  fragColor = vec4(colour, alpha);
}
`;
