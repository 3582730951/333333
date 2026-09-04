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

uniform vec2 uResolution;
uniform vec3 uAtmoGlow;
uniform vec3 uAtmoNear;
uniform vec2 uTilt;
uniform float uDepth;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Parallax sheen for a tilted card. The card itself is DOM and is transformed by
// CSS (D2); this layer only paints the moving specular band so that the sheen and
// the transform agree without WebGL owning any layout.
void main() {
  vec2 centred = vUv - 0.5;
  float along = dot(centred, normalize(uTilt + vec2(0.0001)));
  float band = exp(-pow(along * 3.2 - length(uTilt) * uDepth, 2.0) * 6.0);
  float edge = 1.0 - smoothstep(0.35, 0.5, length(centred * vec2(uResolution.x / max(uResolution.y, 1.0), 1.0)));
  float alpha = band * edge * uIntensity;
  fragColor = vec4(mix(uAtmoNear, uAtmoGlow, band) * alpha, alpha);
}
`;
