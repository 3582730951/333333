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
uniform vec2 uResolution;
uniform vec3 uAtmoGlow;
uniform vec3 uAtmoNear;
uniform vec2 uPointer;
uniform float uPull;
uniform float uRadius;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Magnetic pull: the field is strongest at the pointer and falls off smoothly.
// The button itself is DOM (D2) -- this layer only paints the attraction field
// beneath it, so text stays selectable and focusable.
float field(vec2 uv, vec2 centre, float radius) {
  float d = length((uv - centre) * vec2(uResolution.x / max(uResolution.y, 1.0), 1.0));
  return 1.0 - smoothstep(0.0, max(radius, 0.0001), d);
}

void main() {
  float pull = field(vUv, uPointer, uRadius);
  float shaped = pow(pull, 1.0 + uPull * 2.0);
  float breathe = 0.92 + 0.08 * sin(uTime * 2.1);
  vec3 tint = mix(uAtmoNear, uAtmoGlow, shaped);
  float alpha = shaped * uIntensity * breathe;
  fragColor = vec4(tint * alpha, alpha);
}
`;
