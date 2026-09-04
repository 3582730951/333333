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
uniform float uViscosity;
uniform float uTurbulence;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(127.1, 311.7));
  p += dot(p, p + 19.19);
  return fract(p.x * p.y);
}

float noise(vec2 p) {
  vec2 cell = floor(p);
  vec2 local = fract(p);
  vec2 curve = local * local * (3.0 - 2.0 * local);
  return mix(mix(hash21(cell), hash21(cell + vec2(1.0, 0.0)), curve.x), mix(hash21(cell + vec2(0.0, 1.0)), hash21(cell + 1.0), curve.x), curve.y);
}

float fbm(vec2 p) {
  float total = 0.0;
  float weight = 0.56;
  for (int octave = 0; octave < 3; octave += 1) {
    total += noise(p) * weight;
    p = mat2(1.62, -1.13, 1.13, 1.62) * p + vec2(1.9, -3.7);
    weight *= 0.53;
  }
  return total;
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  vec2 domain = p * (1.5 + uViscosity * 1.7);
  float left = fbm(domain + vec2(time * 0.13, -time * 0.08));
  float right = fbm(domain.yx * 1.31 + vec2(-time * 0.11, time * 0.09));
  vec2 curl = vec2(right - 0.48, 0.48 - left) * uTurbulence;
  float body = fbm(domain + curl * 1.45 + vec2(time * 0.04, time * 0.03));
  float foam = smoothstep(0.49, 0.78, sin((body + left * 0.45) * 15.708) * 0.5 + 0.5);
  float vignette = 1.0 - smoothstep(0.34, 0.98, length(p));
  vec3 deep = mix(uAtmoVoid, uAtmoFar, body * 0.78);
  vec3 current = mix(uAtmoNear, uAtmoGlow, left * 0.65 + right * 0.18);
  vec3 color = mix(deep, current, 0.42 + foam * 0.46);
  float alpha = (0.16 + foam * 0.56) * vignette * uIntensity * mix(0.6, 1.0, uQuality);
  fragColor = vec4(color, alpha);
}
`;
