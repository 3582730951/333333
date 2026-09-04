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
uniform float uDensity;
uniform float uSpeed;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Screen-space particle field. The count is a compile-time constant so the loop
// unrolls on ES 3.0; density scales brightness and cell size instead of the loop
// bound, because a uniform loop bound is a hard compile error here, not a slow path.
const int CELLS = 24;

// Small-constant hash: every literal stays inside mediump range (|x| <= 16384).
vec2 hash2(float n) {
  vec2 p = fract(vec2(n * 231.34, n * 512.77));
  p += dot(p, p + 33.33);
  return fract(p);
}

void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  float total = 0.0;
  for (int i = 0; i < CELLS; i++) {
    float fi = float(i);
    vec2 seed = hash2(fi + 1.0);
    float drift = uTime * uSpeed * (0.4 + seed.y * 0.8);
    vec2 pos = fract(seed + vec2(drift * 0.13, drift * 0.07));
    float d = length((vUv - pos) * aspect);
    total += (1.0 - smoothstep(0.0, 0.012 + uDensity * 0.02, d));
  }
  float alpha = clamp(total, 0.0, 1.0) * uIntensity;
  fragColor = vec4(mix(uAtmoFar, uAtmoGlow, clamp(total, 0.0, 1.0)) * alpha, alpha);
}
`;
