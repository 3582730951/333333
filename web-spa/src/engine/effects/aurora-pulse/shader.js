export const VERTEX_SOURCE = `#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`;

export const FRAGMENT_SOURCE = `#version 300 es
precision mediump float;

in vec2 vUv;

// Injected by Aurora. uTime advances only with the fixed-step clock, so it
// pauses, slows, seeks, and replays with the engine instead of wall time.
uniform float uTime;
uniform float uDeltaTime;
uniform vec2 uResolution;
uniform float uPixelRatio;
uniform float uQuality;
uniform vec3 uAtmoVoid;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;

// Declared by this effect's manifest.uniforms table.
uniform float uIntensity;
uniform float uSpeed;
uniform float uAmplitude;

out vec4 fragColor;

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float radius = length(p);
  float phase = radius * (11.0 + uQuality * 8.0) - uTime * uSpeed;
  float crest = 0.5 + 0.5 * sin(phase);
  float ring = smoothstep(0.84, 0.22, abs(radius - (0.22 + crest * uAmplitude)));
  float edge = smoothstep(0.72, 0.12, radius);
  float alpha = ring * edge * uIntensity;
  fragColor = vec4(uAtmoGlow, alpha);
}
`;
