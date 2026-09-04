// Depth fog is clipped to one panel/card (or a group union rect), never a
// screen-wide pass. The caller maps its DOMRect to normalized uElementRect.
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

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uDensity;
uniform float uFalloff;
uniform float uSpeed;
uniform float uDepth;
uniform float uCornerRadius;
uniform float uDetail;
uniform float uAlphaCap;

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
  float shape = 1.0 - smoothstep(0.0, 0.012, roundedBox(point, vec2(0.5 * aspect, 0.5), radius));
  float verticalDepth = clamp((1.0 - vUv.y) * (0.72 + uDepth * 0.7), 0.0, 1.0);
  float sideDepth = 1.0 - smoothstep(0.18, 0.74, length(point));
  float haze = pow(verticalDepth, max(uFalloff, 0.1)) * (0.46 + sideDepth * 0.54);
  float wisps = 0.5 + 0.5 * sin((vUv.x * 8.0 + vUv.y * 5.0) * uDensity + uTime * uSpeed);
  float fine = 0.5 + 0.5 * sin(vUv.x * 31.0 - vUv.y * 17.0 + uTime * uSpeed * 0.6);
  float veil = mix(wisps, fine, uDetail);
  vec3 color = mix(uAtmoVoid, uAtmoNear, 0.32 + verticalDepth * 0.48);
  color = mix(color, uTint, 0.2 + veil * 0.24);
  float alpha = shape * haze * uIntensity * uAlphaCap * (0.52 + veil * 0.48);
  fragColor = vec4(color, alpha);
}
`;
