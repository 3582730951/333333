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
uniform vec2 uOrigin;
out vec4 fragColor;

void main() {
  vec2 p = vUv - uOrigin;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float radius = length(p);
  float travellingRadius = uProgress * 1.28 * uScale;
  float band = abs(radius - travellingRadius);
  float width = mix(0.035, 0.009, uDensity) / max(uScale, 0.01);
  float mainRing = 1.0 - smoothstep(width, width * 2.2, band);
  float secondaryRadius = max(0.0, travellingRadius - mix(0.08, 0.22, uDensity));
  float secondary = 1.0 - smoothstep(width * 1.4, width * 3.2, abs(radius - secondaryRadius));
  float shimmer = 0.72 + 0.28 * sin(radius * mix(28.0, 76.0, uDensity) - uTime * uSpeed * 4.0);
  float fade = 1.0 - smoothstep(0.78, 1.12, radius);
  vec3 color = mix(uAtmoNear, uTint, 0.82) + uAtmoGlow * 0.2;
  float alpha = (mainRing + secondary * 0.42) * shimmer * fade * uIntensity * mix(0.55, 1.0, uQuality);
  fragColor = vec4(color, alpha);
}
`;
