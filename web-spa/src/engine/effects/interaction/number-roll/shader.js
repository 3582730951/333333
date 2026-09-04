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

uniform vec3 uAtmoGlow;
uniform float uRoll;
uniform float uBlur;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Digit-roll motion cue. D3 forbids WebGL owning text, so the digits stay DOM and
// this layer paints only the vertical motion blur band that sells the roll. The
// number itself is always readable and selectable even with WebGL off.
void main() {
  float travel = fract(uRoll);
  float band = exp(-pow((vUv.y - travel) / max(uBlur, 0.0001), 2.0) * 2.4);
  float settle = 1.0 - smoothstep(0.85, 1.0, travel);
  float alpha = band * settle * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`;
