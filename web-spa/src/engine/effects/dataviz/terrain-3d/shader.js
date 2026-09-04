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
uniform float uHeight;
uniform float uGrid;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Pseudo-3D data terrain via a raked grid: perspective comes from compressing the
// grid toward the horizon, not from a depth buffer, so it stays one fullscreen
// draw with zero geometry upload.
float ridge(vec2 p) {
  return sin(p.x * 3.1 + uTime * 0.18) * cos(p.y * 2.3 - uTime * 0.11);
}

void main() {
  float horizon = 0.62;
  float below = max(vUv.y - horizon, 0.0);
  float perspective = 1.0 / max(below * 6.0 + 0.12, 0.0001);
  vec2 p = vec2((vUv.x - 0.5) * perspective, perspective);
  float h = ridge(p) * uHeight;
  float lines = abs(fract(p.y * uGrid + h) - 0.5);
  float grid = 1.0 - smoothstep(0.0, 0.09, lines);
  float fade = 1.0 - smoothstep(horizon, 1.0, vUv.y);
  float mask = step(horizon, vUv.y);
  float alpha = grid * fade * mask * uIntensity;
  fragColor = vec4(mix(uAtmoNear, uAtmoGlow, grid) * alpha, alpha);
}
`;
