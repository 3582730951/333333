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

in vec2 vUv;
uniform float uTime;
uniform vec2 uResolution;
uniform float uQuality;
uniform vec3 uAtmoGlow;
uniform float uProgress;
uniform vec3 uFromColor;
uniform vec3 uToColor;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(91.7, 47.2));
  return fract(sin(dot(p, vec2(19.3, 71.1))) * 12345.6);
}

void main() {
  vec2 centred = vUv - 0.5;
  float radial = 1.0 - smoothstep(0.12, 0.76, length(centred));
  vec3 routeColour = mix(uFromColor, uToColor, uProgress);
  float cells = mix(4.0, 24.0, uDensity) * uScale * mix(0.6, 1.0, uQuality);
  float grain = (hash21(floor(vUv * cells + uTime * uSpeed * 0.02)) - 0.5) * 0.12;
  vec3 colour = mix(routeColour, uTint, radial * 0.5) + uAtmoGlow * (0.08 + radial * 0.16) + grain;
  float pulse = 0.84 + 0.16 * sin(uProgress * 6.283 + uTime * uSpeed * 2.0);
  float alpha = (0.24 + radial * 0.34) * pulse * uIntensity;
  fragColor = vec4(colour, alpha);
}
`;
