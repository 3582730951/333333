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
uniform vec3 uAtmoGlow;
uniform float uRate;
uniform float uWidth;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Live-data heartbeat: a pulse travelling left to right whenever new samples land.
// uRate is fed from the real arrival rate, so a quiet stream visibly goes quiet
// instead of animating a lie.
void main() {
  float phase = fract(uTime * uRate);
  float band = exp(-pow((vUv.x - phase) / max(uWidth, 0.0001), 2.0) * 4.0);
  float centre = 1.0 - smoothstep(0.0, 0.5, abs(vUv.y - 0.5));
  float alpha = band * centre * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`;
