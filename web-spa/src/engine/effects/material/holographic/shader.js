// The quad is the union box of one DOM element or a coordinated element
// group. The host/caller supplies its normalized DOMRect as uElementRect.
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
uniform vec2 uResolution;
uniform float uPixelRatio;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uStripeScale;
uniform float uIridescence;
uniform float uNoise;
uniform float uCornerRadius;
uniform float uDetail;
uniform float uAlphaCap;

out vec4 fragColor;

float hash12(vec2 point) {
  return fract(sin(dot(point, vec2(41.7, 113.3))) * 157.31);
}

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
  vec2 physical = max(uElementRect.zw * uResolution, vec2(1.0));
  vec2 pixel = vUv * physical;
  float dpr = max(uPixelRatio, 1.0);
  float stripe = 0.5 + 0.5 * sin((pixel.x + pixel.y * 0.46) / max(1.0, uStripeScale * dpr) + uTime * uSpeed);
  float colorPhase = stripe * 6.28318 * uIridescence + uTime * uSpeed * 0.35;
  float sparkle = hash12(floor(pixel / max(1.0, dpr * 2.0)) + floor(uTime * 2.0));
  vec3 first = mix(uAtmoNear, uTint, 0.3 + 0.7 * stripe);
  vec3 second = mix(uAtmoGlow, uAtmoFar, 0.35 + 0.55 * (1.0 - stripe));
  vec3 color = mix(first, second, 0.5 + 0.5 * sin(colorPhase));
  color += (sparkle - 0.5) * uNoise * uDetail;
  float edge = pow(1.0 - clamp(length(point) * 1.4, 0.0, 1.0), 2.0);
  float alpha = shape * uIntensity * uAlphaCap * (0.34 + stripe * 0.34 + edge * 0.32);
  fragColor = vec4(max(color, vec3(0.0)), alpha);
}
`;
