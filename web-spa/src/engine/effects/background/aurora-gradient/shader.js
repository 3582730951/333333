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
uniform float uSpread;
uniform float uDrift;
out vec4 fragColor;

float curtain(vec2 p, float lane, float time) {
  float frequency = 1.7 + lane * 0.52;
  float wobble = sin(p.x * frequency + time * (0.72 + lane * 0.19));
  wobble += sin(p.x * (frequency * 2.1) - time * 0.43 + lane) * 0.26;
  float center = 0.13 + lane * 0.22 + wobble * uDrift * 0.16;
  float distanceFromBand = abs(p.y - center);
  return 1.0 - smoothstep(0.02, 0.12 + uSpread * 0.22, distanceFromBand);
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  float light = 0.0;
  for (int lane = 0; lane < 3; lane += 1) {
    float laneValue = float(lane);
    float visibility = 1.0 - smoothstep(0.45, 1.0, laneValue * (1.0 - uQuality));
    light += curtain(p, laneValue, time) * visibility * (1.0 - laneValue * 0.18);
  }
  float edge = 1.0 - smoothstep(0.38, 0.98, length(p * vec2(0.72, 1.0)));
  float veil = smoothstep(-0.5, 0.42, p.y + sin(p.x * 1.3 + time * 0.25) * 0.08);
  vec3 backdrop = mix(uAtmoVoid, uAtmoFar, veil * 0.54);
  vec3 curtainColor = mix(uAtmoNear, uAtmoGlow, 0.36 + 0.38 * sin(time * 0.18 + p.x));
  vec3 color = mix(backdrop, curtainColor, clamp(light, 0.0, 1.0));
  float alpha = clamp(light * edge * uIntensity, 0.0, 0.72);
  fragColor = vec4(color, alpha);
}
`;
