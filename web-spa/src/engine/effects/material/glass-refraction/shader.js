// Element-scoped geometry: uElementRect is [left, bottom, width, height] in
// normalized canvas coordinates. The vertex shader rasterizes only that quad.
export const VERTEX_SOURCE = `#version 300 es
precision mediump float;

uniform vec4 uElementRect;
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  vec2 position = uElementRect.xy + corner * uElementRect.zw;
  gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}
`;

export const FRAGMENT_SOURCE = `#version 300 es
precision mediump float;

in vec2 vUv;
uniform float uTime;
uniform vec3 uAtmoVoid;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uRefraction;
uniform float uBlur;
uniform float uCornerRadius;
uniform float uWaveDensity;
uniform float uOpacityCap;

out vec4 fragColor;

float roundedBox(vec2 point, vec2 halfSize, float radius) {
  vec2 q = abs(point) - halfSize + radius;
  return length(max(q, 0.0)) + min(max(q.x, q.y), 0.0) - radius;
}

void main() {
  float aspect = uElementRect.z / max(uElementRect.w, 0.0001);
  vec2 point = vUv - 0.5;
  point.x *= aspect;
  float radius = min(uCornerRadius, min(0.49, 0.49 * aspect));
  float distanceToCorner = roundedBox(point, vec2(0.5 * aspect, 0.5), radius);
  float shape = 1.0 - smoothstep(0.0, 0.012, distanceToCorner);

  float phase = uTime * uSpeed;
  vec2 normal = vec2(
    sin((point.y * 19.0 + phase) * uWaveDensity),
    cos((point.x * 17.0 - phase * 0.8) * uWaveDensity)
  );
  vec2 refracted = clamp(vUv + normal * uRefraction, 0.0, 1.0);
  vec3 horizon = mix(uAtmoVoid, uAtmoNear, refracted.y);
  vec3 glow = mix(uAtmoFar, uAtmoGlow, refracted.x);
  float cloud = 0.5 + 0.5 * sin((refracted.x + refracted.y) * 8.0 + phase * 0.35);
  vec3 proxyBackdrop = mix(horizon, glow, mix(0.28, 0.72, cloud * uBlur));
  float fresnel = pow(1.0 - clamp(length(point) * 1.3, 0.0, 1.0), 2.4);
  vec3 color = mix(proxyBackdrop, uTint, 0.18 + fresnel * 0.32);
  float alpha = shape * uIntensity * uOpacityCap * (0.62 + fresnel * 0.38);
  fragColor = vec4(color, alpha);
}
`;
