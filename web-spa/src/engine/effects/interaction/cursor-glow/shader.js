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
uniform vec3 uAtmoFar;
uniform vec2 uPointer;
uniform float uRadius;
uniform float uIntensity;
uniform float uTrail;

in vec2 vUv;
out vec4 fragColor;

// Two-lobe halo: a tight core that reads as "the cursor is here" and a wide,
// slower lobe that reads as "it came from over there". The trail is faked in the
// falloff rather than by keeping history -- steady-state zero allocation (R3).
void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  float d = length((vUv - uPointer) * aspect);
  float core = 1.0 - smoothstep(0.0, max(uRadius, 0.0001), d);
  float wide = 1.0 - smoothstep(0.0, max(uRadius * 3.0, 0.0001), d);
  float pulse = 0.94 + 0.06 * sin(uTime * 3.4);
  float alpha = (core * 0.75 + wide * uTrail * 0.35) * uIntensity * pulse;
  vec3 tint = mix(uAtmoFar, uAtmoGlow, core);
  fragColor = vec4(tint * alpha, alpha);
}
`;
