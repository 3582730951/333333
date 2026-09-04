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
uniform float uSpread;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Success burst. The particle count is a compile-time constant so the loop is
// unrollable on ES 3.0 -- a uniform bound here would be a hard compile error, not
// a slow path.
const int PARTICLES = 12;

// Small-constant hash: keeps every literal inside the mediump range (|x| <= 16384)
// so the shader does not need highp. Same idiom as background/star-parallax.
float hash(float n) {
  float p = fract(n * 231.34);
  p += p * (p + 33.33);
  return fract(p * 512.77);
}

void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  float total = 0.0;
  for (int i = 0; i < PARTICLES; i++) {
    float fi = float(i);
    float angle = hash(fi) * 6.28318;
    float speed = 0.45 + hash(fi + 7.0) * 0.55;
    vec2 offset = vec2(cos(angle), sin(angle)) * uProgress * speed * uSpread;
    float d = length((vUv - uOrigin - offset) * aspect);
    total += (1.0 - smoothstep(0.0, 0.022, d));
  }
  float fade = 1.0 - uProgress;
  float alpha = clamp(total, 0.0, 1.0) * fade * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`;
