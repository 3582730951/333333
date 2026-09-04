// The fullscreen VAO is repurposed as an element quad, avoiding a fullscreen
// fragment pass for a material that only decorates one DOM surface.
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
uniform vec3 uAtmoGlow;
uniform vec3 uAtmoFar;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uBorderWidth;
uniform float uSoftness;
uniform float uCornerRadius;
uniform float uSegmentDensity;
uniform float uShineStrength;
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
  float distanceToEdge = abs(roundedBox(point, vec2(0.5 * aspect, 0.5), radius));
  float core = 1.0 - smoothstep(0.0, uBorderWidth, distanceToEdge);
  float halo = 1.0 - smoothstep(uBorderWidth, uBorderWidth + uSoftness, distanceToEdge);
  float sweep = 0.5 + 0.5 * sin((vUv.x - vUv.y) * 18.0 * uSegmentDensity + uTime * uSpeed);
  vec3 base = mix(uAtmoFar, uAtmoGlow, sweep * uShineStrength);
  vec3 color = mix(base, uTint, 0.55 + sweep * 0.25);
  float alpha = max(core, halo * 0.62) * uIntensity * uAlphaCap;
  fragColor = vec4(color, alpha);
}
`;
