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

uniform float uTime;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoGlow;
uniform float uSpeed;
uniform float uWidth;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Loading shimmer. It must be able to stop: prefers-reduced-motion is handled by
// the host dropping this effect entirely, and the DOM skeleton underneath stays
// visible on its own, so the loading state never depends on WebGL (R1).
void main() {
  float sweep = fract(uTime * uSpeed);
  float pos = sweep * 1.6 - 0.3;
  float band = exp(-pow((vUv.x + vUv.y * 0.35 - pos) / max(uWidth, 0.0001), 2.0));
  float alpha = band * uIntensity;
  fragColor = vec4(mix(uAtmoNear, uAtmoGlow, band) * alpha, alpha);
}
`;
