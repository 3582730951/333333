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

uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;
uniform float uVelocity;
uniform float uOverscroll;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Scroll inertia readout: a velocity smear plus an overscroll cushion at the
// edge being pushed against. The list itself scrolls natively in the DOM (D2) --
// this only paints the physical cue, so keyboard and screen-reader scrolling are
// untouched.
void main() {
  float dir = sign(uVelocity);
  float speed = clamp(abs(uVelocity), 0.0, 1.0);
  float edge = dir > 0.0 ? vUv.y : 1.0 - vUv.y;
  float smear = (1.0 - smoothstep(0.0, 0.45, edge)) * speed;
  float cushion = (1.0 - smoothstep(0.0, 0.22, edge)) * abs(uOverscroll);
  float alpha = (smear * 0.45 + cushion * 0.8) * uIntensity;
  fragColor = vec4(mix(uAtmoFar, uAtmoGlow, cushion) * alpha, alpha);
}
`;
