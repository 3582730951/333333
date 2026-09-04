export const VERTEX_SOURCE = `#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`;

export const FRAGMENT_SOURCE = `#version 300 es
precision highp float;

in vec2 vUv;
uniform float uTime;
uniform float uDeltaTime;
uniform vec2 uResolution;
uniform float uPixelRatio;
uniform float uQuality;
uniform vec3 uAtmoVoid;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;
uniform float uIntensity;
uniform float uSpeed;
uniform float uScale;
uniform float uSpacing;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(89.1, 221.7));
  p += dot(p, p + 17.4);
  return fract(p.x * p.y);
}

float terrain(vec2 p) {
  vec2 cell = floor(p);
  vec2 local = fract(p);
  vec2 curve = local * local * (3.0 - 2.0 * local);
  float a = hash21(cell);
  float b = hash21(cell + vec2(1.0, 0.0));
  float c = hash21(cell + vec2(0.0, 1.0));
  float d = hash21(cell + vec2(1.0, 1.0));
  return mix(mix(a, b, curve.x), mix(c, d, curve.x), curve.y);
}

float mapHeight(vec2 p) {
  float value = 0.0;
  float weight = 0.6;
  for (int octave = 0; octave < 3; octave += 1) {
    value += terrain(p) * weight;
    p = mat2(1.7, -1.14, 1.14, 1.7) * p + vec2(2.7, -3.2);
    weight *= 0.48;
  }
  return value;
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  vec2 domain = p * (2.1 + uScale * 2.3) + vec2(time * 0.06, -time * 0.04);
  float height = mapHeight(domain);
  float interval = max(uSpacing * (1.18 - uQuality * 0.22), 0.025);
  float phase = abs(fract(height / interval) - 0.5);
  float line = 1.0 - smoothstep(0.34, 0.48, phase);
  float index = floor(height / interval);
  float major = 1.0 - smoothstep(0.0, 0.06, abs(fract(index * 0.2) - 0.5));
  float edge = 1.0 - smoothstep(0.3, 1.04, length(p));
  vec3 base = mix(uAtmoVoid, uAtmoFar, height * 0.58);
  vec3 ink = mix(uAtmoNear, uAtmoGlow, major * 0.62 + line * 0.2);
  vec3 color = mix(base, ink, line * (0.62 + major * 0.28));
  float alpha = (0.08 + line * 0.7 + major * line * 0.22) * edge * uIntensity;
  fragColor = vec4(color, alpha);
}
`;
