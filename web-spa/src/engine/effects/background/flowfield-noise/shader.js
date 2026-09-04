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
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(123.34, 456.21));
  p += dot(p, p + 45.32);
  return fract(p.x * p.y);
}

float valueNoise(vec2 p) {
  vec2 cell = floor(p);
  vec2 local = fract(p);
  vec2 curve = local * local * (3.0 - 2.0 * local);
  float a = hash21(cell);
  float b = hash21(cell + vec2(1.0, 0.0));
  float c = hash21(cell + vec2(0.0, 1.0));
  float d = hash21(cell + vec2(1.0, 1.0));
  return mix(mix(a, b, curve.x), mix(c, d, curve.x), curve.y);
}

float field(vec2 p) {
  float value = 0.0;
  float weight = 0.58;
  for (int octave = 0; octave < 3; octave += 1) {
    value += valueNoise(p) * weight;
    p = mat2(1.72, -1.19, 1.19, 1.72) * p + vec2(3.1, -2.4);
    weight *= 0.52;
  }
  return value;
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  vec2 domain = p * (1.55 + uScale * 1.8);
  float first = field(domain + vec2(time * 0.13, -time * 0.09));
  float second = field(domain.yx * 1.42 + vec2(-time * 0.08, time * 0.11));
  float flow = fract(first + second * 0.72 + length(p) * 0.18);
  float filaments = smoothstep(0.58, 0.91, sin(flow * 18.8496) * 0.5 + 0.5);
  float vignette = 1.0 - smoothstep(0.28, 0.94, length(p));
  float qualityFade = mix(0.62, 1.0, clamp(uQuality, 0.0, 1.0));
  vec3 body = mix(uAtmoVoid, uAtmoNear, first * 0.9);
  vec3 hue = mix(uAtmoFar, uAtmoGlow, second * 0.78);
  vec3 color = mix(body, hue, filaments * 0.72);
  float alpha = (0.16 + filaments * 0.66) * vignette * qualityFade * uIntensity;
  fragColor = vec4(color, alpha);
}
`;
