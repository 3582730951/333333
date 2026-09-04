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
uniform float uDensity;
uniform float uParallax;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(231.34, 512.77));
  p += dot(p, p + 33.33);
  return fract(p.x * p.y);
}

float starLayer(vec2 p, float scale, float time) {
  vec2 cell = floor(p * scale);
  vec2 local = fract(p * scale) - 0.5;
  float seed = hash21(cell);
  vec2 point = vec2(hash21(cell + 7.1), hash21(cell + 13.7)) - 0.5;
  float distanceToStar = length(local - point * 0.78);
  float radius = mix(0.025, 0.105, seed * seed);
  float sparkle = 0.72 + 0.28 * sin(time * (1.4 + seed * 2.2) + seed * 37.0);
  return (1.0 - smoothstep(radius * 0.45, radius, distanceToStar)) * sparkle;
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  float stars = 0.0;
  for (int layer = 0; layer < 3; layer += 1) {
    float depth = float(layer) + 1.0;
    float visibility = 1.0 - smoothstep(0.42, 1.0, float(layer) * (1.0 - uQuality));
    vec2 shift = vec2(time * (0.018 + depth * 0.014) + uParallax * depth * 0.055, -time * depth * 0.009);
    stars += starLayer(p + shift, (3.8 + depth * 3.4) * uDensity, time) * visibility * (0.62 + depth * 0.14);
  }
  float mist = smoothstep(1.06, 0.16, length(p + vec2(0.1, -0.08)));
  vec3 space = mix(uAtmoVoid, uAtmoFar, mist * 0.38);
  vec3 starColor = mix(uAtmoNear, uAtmoGlow, clamp(stars, 0.0, 1.0));
  vec3 color = mix(space, starColor, clamp(stars * 0.86, 0.0, 1.0));
  float alpha = min(0.78, (0.08 * mist + stars * 0.86) * uIntensity);
  fragColor = vec4(color, alpha);
}
`;
