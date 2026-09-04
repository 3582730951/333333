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
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;
uniform float uNodes;
uniform float uLinkWidth;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Force-directed link glow. Node positions are procedural rather than uploaded so
// this stays a one-draw ambient layer; the authoritative graph is DOM/SVG above it
// and remains keyboard-navigable (R2).
const int NODES = 10;

vec2 nodeAt(int i, float t) {
  float fi = float(i);
  float a = fi * 2.39996 + t * 0.12;
  float r = 0.18 + fract(fi * 91.7 * 0.137) * 0.26;
  return vec2(0.5, 0.5) + vec2(cos(a), sin(a) * 0.72) * r;
}

float segment(vec2 p, vec2 a, vec2 b) {
  vec2 pa = p - a;
  vec2 ba = b - a;
  float h = clamp(dot(pa, ba) / max(dot(ba, ba), 0.0001), 0.0, 1.0);
  return length(pa - ba * h);
}

void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  vec2 p = vUv * aspect;
  float glow = 0.0;
  for (int i = 0; i < NODES; i++) {
    vec2 a = nodeAt(i, uTime) * aspect;
    vec2 b = nodeAt((i + 3) % NODES, uTime) * aspect;
    float d = segment(p, a, b);
    glow += 1.0 - smoothstep(0.0, uLinkWidth, d);
  }
  float shaped = clamp(glow * (uNodes / float(NODES)), 0.0, 1.0);
  float alpha = shaped * uIntensity;
  fragColor = vec4(mix(uAtmoFar, uAtmoGlow, shaped) * alpha, alpha);
}
`;
