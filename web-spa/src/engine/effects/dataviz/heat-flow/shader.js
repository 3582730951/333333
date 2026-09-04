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
uniform vec3 uAtmoNear;
uniform vec3 uAtmoGlow;
uniform float uScale;
uniform float uSpeed;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Heat field over a data surface. Deliberately monotonic in luminance so it stays
// readable under both dichromacies -- hue alone must never carry the value, which
// is why the ramp mixes toward glow rather than rotating hue.
//
// Small-constant value noise: no literal exceeds the mediump range (|x| <= 16384),
// so this stays mediump on mobile GPUs. Same idiom as background/star-parallax.
float hash21(vec2 p) {
  p = fract(p * vec2(231.34, 512.77));
  p += dot(p, p + 33.33);
  return fract(p.x * p.y);
}

float noise(vec2 p) {
  vec2 i = floor(p);
  vec2 f = fract(p);
  vec2 u = f * f * (3.0 - 2.0 * f);
  float a = hash21(i);
  float b = hash21(i + vec2(1.0, 0.0));
  float c = hash21(i + vec2(0.0, 1.0));
  float d = hash21(i + vec2(1.0, 1.0));
  return mix(mix(a, b, u.x), mix(c, d, u.x), u.y);
}

void main() {
  vec2 p = vUv * uScale + vec2(uTime * uSpeed * 0.15, uTime * uSpeed * 0.09);
  float heat = noise(p) * 0.6 + noise(p * 2.1) * 0.4;
  float alpha = heat * uIntensity;
  fragColor = vec4(mix(uAtmoNear, uAtmoGlow, heat) * alpha, alpha);
}
`;
