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
uniform vec2 uOrigin;
uniform vec4 uFromRect;
uniform vec4 uToRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
uniform float uCornerRadius;
out vec4 fragColor;

float roundedBox(vec2 point, vec2 halfSize, float radius) {
  vec2 q = abs(point) - halfSize + radius;
  return length(max(q, vec2(0.0))) + min(max(q.x, q.y), 0.0) - radius;
}

void main() {
  vec4 rect = mix(uFromRect, uToRect, uProgress);
  vec2 centre = rect.xy + rect.zw * 0.5;
  centre = mix(uOrigin, centre, smoothstep(0.0, 1.0, uProgress));
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  vec2 point = (vUv - centre) * aspect;
  vec2 halfSize = max(rect.zw * 0.5 * aspect, vec2(0.001));
  float radius = min(min(halfSize.x, halfSize.y) * uCornerRadius * 2.0, 0.45);
  float distanceToBox = roundedBox(point, halfSize, radius);
  float width = mix(0.003, 0.013, uDensity) * uScale;
  float outline = 1.0 - smoothstep(width, width * 2.3, abs(distanceToBox));
  float interior = 1.0 - smoothstep(0.0, width * 2.0, distanceToBox);
  float shimmer = 0.86 + 0.14 * sin((point.x - point.y) * mix(18.0, 60.0, uDensity) + uTime * uSpeed * 3.0);
  vec3 colour = mix(uTint, uAtmoGlow, 0.2);
  float alpha = (outline * 0.84 + interior * 0.06) * shimmer * uIntensity * (0.58 + 0.42 * uQuality);
  fragColor = vec4(colour, alpha);
}
`;
