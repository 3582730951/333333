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
uniform vec2 uOrigin;
uniform float uPress;
uniform float uSpring;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Elastic press: a compression ring that overshoots once and settles. uPress is
// driven by the host from pointerdown/up, so the visual is same-frame with the
// input (R4) -- the shader never waits on an animation clock of its own.
void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  float d = length((vUv - uOrigin) * aspect);
  float overshoot = sin(uPress * 3.14159) * uSpring;
  float radius = 0.16 + overshoot * 0.09;
  float ring = 1.0 - smoothstep(radius * 0.55, radius, d);
  float rim = smoothstep(radius * 0.72, radius, d) * (1.0 - smoothstep(radius, radius * 1.25, d));
  float alpha = (ring * 0.35 + rim * 0.9) * uPress * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`;
