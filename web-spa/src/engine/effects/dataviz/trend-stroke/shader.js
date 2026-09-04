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
uniform float uProgress;
uniform float uThickness;
uniform float uAmplitude;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Trend-line draw-on. The real series is an SVG path in the DOM; this paints the
// travelling highlight only, so the chart is complete and readable the instant it
// renders even when the effect never loads.
void main() {
  float curve = 0.5 + sin(vUv.x * 9.0) * 0.13 * uAmplitude + sin(vUv.x * 3.1) * 0.07 * uAmplitude;
  float line = 1.0 - smoothstep(0.0, uThickness, abs(vUv.y - curve));
  float drawn = 1.0 - smoothstep(uProgress - 0.04, uProgress, vUv.x);
  float head = exp(-pow((vUv.x - uProgress) / 0.05, 2.0) * 2.0);
  float alpha = line * (drawn * 0.55 + head) * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`;
