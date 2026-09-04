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
uniform float uBreath;
out vec4 fragColor;

float orb(vec2 p, vec2 center, float radius) {
  float distanceToCenter = length(p - center);
  return 1.0 - smoothstep(radius * 0.52, radius, distanceToCenter);
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  float total = 0.0;
  vec3 color = uAtmoVoid;
  for (int index = 0; index < 4; index += 1) {
    float id = float(index);
    float phase = time * (0.62 + id * 0.13) + id * 2.1;
    vec2 center = vec2(
      sin(phase * 0.73 + id) * (0.22 + id * 0.045),
      cos(phase * 0.61 - id * 0.7) * (0.18 + id * 0.035)
    );
    float pulse = 1.0 + sin(time * (1.1 + id * 0.18) + id * 1.7) * uBreath * 0.18;
    float contribution = orb(p * (1.0 + id * 0.06) / uScale, center, (0.11 + id * 0.024) * pulse);
    float visibility = 1.0 - smoothstep(0.42, 1.0, id * (1.0 - uQuality));
    total += contribution * visibility;
    color += mix(uAtmoNear, uAtmoGlow, 0.3 + id * 0.16) * contribution * visibility;
  }
  float haze = smoothstep(0.98, 0.06, length(p)) * 0.08;
  color = mix(mix(uAtmoVoid, uAtmoFar, haze), color, clamp(total * 0.62, 0.0, 1.0));
  float alpha = min(0.78, (haze + total * 0.52) * uIntensity);
  fragColor = vec4(color, alpha);
}
`;
