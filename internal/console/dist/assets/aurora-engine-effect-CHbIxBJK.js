import"./aurora-engine-contracts-CjO_kDw4.js";var e=`#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,t=`#version 300 es
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
uniform float uSpacing;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(89.1, 221.7));
  p += dot(p, p + 17.4);
  return fract(p.x * p.y);
}

float terrain(vec2 p) {
  vec2 cell = floor(p);
  vec2 local = fract(p);
  vec2 curve = local * local * (3.0 - 2.0 * local);
  float a = hash21(cell);
  float b = hash21(cell + vec2(1.0, 0.0));
  float c = hash21(cell + vec2(0.0, 1.0));
  float d = hash21(cell + vec2(1.0, 1.0));
  return mix(mix(a, b, curve.x), mix(c, d, curve.x), curve.y);
}

float mapHeight(vec2 p) {
  float value = 0.0;
  float weight = 0.6;
  for (int octave = 0; octave < 3; octave += 1) {
    value += terrain(p) * weight;
    p = mat2(1.7, -1.14, 1.14, 1.7) * p + vec2(2.7, -3.2);
    weight *= 0.48;
  }
  return value;
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  vec2 domain = p * (2.1 + uScale * 2.3) + vec2(time * 0.06, -time * 0.04);
  float height = mapHeight(domain);
  float interval = max(uSpacing * (1.18 - uQuality * 0.22), 0.025);
  float phase = abs(fract(height / interval) - 0.5);
  float line = 1.0 - smoothstep(0.34, 0.48, phase);
  float index = floor(height / interval);
  float major = 1.0 - smoothstep(0.0, 0.06, abs(fract(index * 0.2) - 0.5));
  float edge = 1.0 - smoothstep(0.3, 1.04, length(p));
  vec3 base = mix(uAtmoVoid, uAtmoFar, height * 0.58);
  vec3 ink = mix(uAtmoNear, uAtmoGlow, major * 0.62 + line * 0.2);
  vec3 color = mix(base, ink, line * (0.62 + major * 0.28));
  float alpha = (0.08 + line * 0.7 + major * line * 0.22) * edge * uIntensity;
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`contour-lines`,title:`Contour Lines`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:19,priority:19,exclusiveGroup:`background-ambient`}),uniforms:Object.freeze({uIntensity:Object.freeze({type:`float`,default:.16,min:0,max:.42,step:.01,description:`Topographic line opacity.`}),uSpeed:Object.freeze({type:`float`,default:.12,min:0,max:.8,step:.01,description:`Fixed-clock contour drift.`}),uScale:Object.freeze({type:`float`,default:1.12,min:.5,max:2.4,step:.01,description:`Contour map scale.`}),uSpacing:Object.freeze({type:`float`,default:.12,min:.04,max:.3,step:.005,description:`Contour interval.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,detailScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,detailScale:.76,alphaCap:.8}),low:Object.freeze({renderScale:.5,detailScale:.52,alphaCap:.58})}),cost:Object.freeze({budgetUnits:Object.freeze({high:2.4,medium:1.5,low:.65}),gpuMilliseconds:Object.freeze({high:.95,medium:.57,low:.3}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n){let r=1-.001**Math.max(0,Math.min(n,.25));return e+(t-e)*r}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=Object.keys(n.uniforms),u=Object.create(null),d=Object.create(null),f=Object.create(null);for(let e=0;e<l.length;e+=1){let t=l[e];u[t]=s.getUniformLocation(c.program,t),d[t]=r(o[t],n.uniforms[t]),f[t]=d[t]}let p=n.quality.medium,m=!0,h=!1;function g(e={}){for(let t=0;t<l.length;t+=1){let i=l[t];Object.prototype.hasOwnProperty.call(e,i)&&(d[i]=r(e[i],n.uniforms[i]))}}function _(e,t){p=n.quality[e]||n.quality.medium,m=!!t}function v(){}function y(e){for(let t=0;t<l.length;t+=1){let n=l[t];f[n]=i(f[n],d[n],e)}}function b(e){if(!(h||!m)){s.useProgram(c.program),a.bindEngineGlobals(c,e);for(let e=0;e<l.length;e+=1){let t=l[e],n=u[t];if(n===null)continue;let r=f[t];t===`uIntensity`&&(r*=p.alphaCap),t===`uScale`&&(r*=p.detailScale),s.uniform1f(n,r)}s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3)}}function x(){h||(h=!0,a.disposeProgram(c))}return{setParameters:g,setQuality:_,resize:v,simulate:y,render:b,dispose:x}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-aurora-contour-lines-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};