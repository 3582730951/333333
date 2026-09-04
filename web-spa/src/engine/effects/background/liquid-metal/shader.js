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
uniform float uRoughness;
uniform float uDistortion;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(91.7, 173.3));
  p += dot(p, p + 27.1);
  return fract(p.x * p.y);
}

float noise(vec2 p) {
  vec2 cell = floor(p);
  vec2 local = fract(p);
  vec2 curve = local * local * (3.0 - 2.0 * local);
  float a = hash21(cell);
  float b = hash21(cell + vec2(1.0, 0.0));
  float c = hash21(cell + vec2(0.0, 1.0));
  float d = hash21(cell + vec2(1.0, 1.0));
  return mix(mix(a, b, curve.x), mix(c, d, curve.x), curve.y);
}

float layered(vec2 p) {
  float value = 0.0;
  float weight = 0.62;
  for (int octave = 0; octave < 3; octave += 1) {
    value += noise(p) * weight;
    p = mat2(1.56, -1.07, 1.07, 1.56) * p + vec2(2.2, -1.6);
    weight *= 0.48;
  }
  return value;
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  vec2 domain = p * 2.8;
  float base = layered(domain + vec2(time * 0.12, -time * 0.08));
  float detail = layered(domain * 1.7 + vec2(-time * 0.07, time * 0.11));
  vec2 normal = vec2(detail - 0.45, base - 0.45) * uDistortion;
  float reflection = layered(domain + normal * 2.6 + vec2(time * 0.03));
  float highlight = pow(max(0.0, 1.0 - abs(reflection * 2.0 - 1.0)), mix(2.0, 7.0, uRoughness));
  float edge = 1.0 - smoothstep(0.34, 1.0, length(p));
  vec3 dark = mix(uAtmoVoid, uAtmoFar, base * 0.74);
  vec3 metal = mix(uAtmoNear, uAtmoGlow, highlight * 0.78 + detail * 0.18);
  vec3 color = mix(dark, metal, 0.34 + highlight * 0.5);
  float alpha = (0.11 + highlight * 0.65) * edge * uIntensity;
  fragColor = vec4(color, alpha);
}
`;
