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

uniform vec2 uResolution;
uniform vec3 uAtmoGlow;
uniform vec2 uOrigin;
uniform float uProgress;
uniform float uWidth;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Material-style click ripple. uProgress is a 0..1 envelope owned by the host so
// the ring starts on the same frame as pointerdown; the shader holds no clock.
void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  float d = length((vUv - uOrigin) * aspect);
  float radius = uProgress * 1.15;
  float band = smoothstep(radius - uWidth, radius, d) * (1.0 - smoothstep(radius, radius + uWidth, d));
  float fade = 1.0 - uProgress;
  float alpha = band * fade * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`;
