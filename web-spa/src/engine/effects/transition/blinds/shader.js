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
uniform vec3 uAtmoNear;
uniform vec3 uAtmoGlow;
uniform float uProgress;
uniform float uOrientation;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
uniform float uSoftness;
out vec4 fragColor;

void main() {
  vec2 p = vUv;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float axis = mix(vUv.y, vUv.x, step(0.5, uOrientation));
  float count = mix(7.0, 34.0, uDensity) * uScale;
  float slatPosition = fract(axis * count);
  float slatIndex = floor(axis * count);
  float stagger = fract(slatIndex * 0.618);
  float threshold = uProgress + (stagger - 0.5) * 0.18;
  float reveal = smoothstep(threshold - uSoftness, threshold + uSoftness, slatPosition);
  float edge = 1.0 - smoothstep(0.0, uSoftness * 2.4, abs(slatPosition - threshold));
  float shimmer = 0.86 + 0.14 * sin(slatIndex * 1.7 + uTime * uSpeed * 3.0);
  float vignette = 1.0 - smoothstep(0.25, 0.78, length(p - vec2(0.5, 0.5)));
  vec3 color = mix(uAtmoNear, uTint, 0.75) + uAtmoGlow * edge * 0.16;
  float alpha = (1.0 - reveal) * (0.22 + edge * 0.78) * shimmer * vignette * uIntensity;
  fragColor = vec4(color, alpha);
}
`;
