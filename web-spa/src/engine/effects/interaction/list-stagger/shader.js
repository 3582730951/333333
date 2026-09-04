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

uniform vec3 uAtmoNear;
uniform vec3 uAtmoGlow;
uniform float uProgress;
uniform float uRows;
uniform float uStagger;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Staggered list entrance. Each row's local envelope is derived from its y band,
// so one draw covers the whole list instead of one effect instance per row --
// that is the difference between 1 draw call and 40.
void main() {
  float rows = max(uRows, 1.0);
  float row = floor(vUv.y * rows);
  float delay = row * uStagger / rows;
  float local = clamp((uProgress - delay) / max(1.0 - delay, 0.0001), 0.0, 1.0);
  float rise = 1.0 - local;
  float edge = 1.0 - smoothstep(0.0, 0.35, fract(vUv.y * rows));
  float alpha = rise * edge * uIntensity;
  fragColor = vec4(mix(uAtmoNear, uAtmoGlow, local) * alpha, alpha);
}
`;
