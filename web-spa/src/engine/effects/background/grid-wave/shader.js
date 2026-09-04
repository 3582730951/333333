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
uniform float uWave;
out vec4 fragColor;

float gridLine(float coordinate, float thickness) {
  float distanceToCenter = abs(fract(coordinate) - 0.5);
  return 1.0 - smoothstep(thickness, thickness + 0.024, distanceToCenter);
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  float waveA = sin(p.x * 5.4 + time * 1.6) * uWave * 0.09;
  float waveB = cos(p.y * 4.7 - time * 1.2) * uWave * 0.07;
  vec2 warped = p + vec2(waveB, waveA);
  vec2 cells = warped * (6.5 + uDensity * 8.0);
  float thickness = mix(0.012, 0.021, uQuality);
  float lines = max(gridLine(cells.x, thickness), gridLine(cells.y, thickness));
  float horizon = smoothstep(-0.5, 0.44, p.y + waveA * 1.8);
  float fade = 1.0 - smoothstep(0.42, 1.08, length(p));
  vec3 field = mix(uAtmoVoid, uAtmoFar, horizon * 0.32);
  vec3 lineColor = mix(uAtmoNear, uAtmoGlow, 0.54 + 0.28 * sin(time * 0.35 + p.x));
  vec3 color = mix(field, lineColor, lines);
  float alpha = (0.09 * horizon + lines * 0.78) * fade * uIntensity;
  fragColor = vec4(color, alpha);
}
`;
