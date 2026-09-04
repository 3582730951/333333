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
uniform float uTime;
uniform vec2 uResolution;
uniform float uQuality;
uniform vec3 uAtmoGlow;
uniform float uProgress;
uniform vec2 uDirection;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
uniform float uDistance;
uniform float uSoftness;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(53.1, 91.7));
  return fract(sin(dot(p, vec2(17.2, 43.8))) * 2342.17);
}

void main() {
  vec2 direction = normalize(uDirection + vec2(0.0001));
  vec2 centred = vUv - 0.5;
  float travel = dot(centred, direction) + 0.5;
  float edgePosition = uProgress * uDistance;
  float leading = 1.0 - smoothstep(edgePosition - uSoftness, edgePosition + uSoftness, travel);
  float trailing = smoothstep(edgePosition - 0.36, edgePosition + 0.02, travel);
  float streakBand = leading * trailing;
  float cells = mix(5.0, 34.0, uDensity) * uScale * mix(0.6, 1.0, uQuality);
  float grain = 0.82 + 0.18 * sin(floor(travel * cells) + uTime * uSpeed * 3.0 + hash21(vUv) * 2.0);
  float vignette = 1.0 - smoothstep(0.2, 0.82, length(centred));
  vec3 colour = mix(uTint, uAtmoGlow, 0.2);
  float alpha = streakBand * grain * vignette * uIntensity;
  fragColor = vec4(colour, alpha);
}
`;
