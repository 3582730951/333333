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
uniform float uViscosity;
uniform float uTurbulence;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(127.1, 311.7));
  p += dot(p, p + 19.19);
  return fract(p.x * p.y);
}

float noise(vec2 p) {
  vec2 cell = floor(p);
  vec2 local = fract(p);
  vec2 curve = local * local * (3.0 - 2.0 * local);
  return mix(mix(hash21(cell), hash21(cell + vec2(1.0, 0.0)), curve.x), mix(hash21(cell + vec2(0.0, 1.0)), hash21(cell + 1.0), curve.x), curve.y);
}

float fbm(vec2 p) {
  float total = 0.0;
  float weight = 0.56;
  for (int octave = 0; octave < 3; octave += 1) {
    total += noise(p) * weight;
    p = mat2(1.62, -1.13, 1.13, 1.62) * p + vec2(1.9, -3.7);
    weight *= 0.53;
  }
  return total;
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  vec2 domain = p * (1.5 + uViscosity * 1.7);
  float left = fbm(domain + vec2(time * 0.13, -time * 0.08));
  float right = fbm(domain.yx * 1.31 + vec2(-time * 0.11, time * 0.09));
  vec2 curl = vec2(right - 0.48, 0.48 - left) * uTurbulence;
  float body = fbm(domain + curl * 1.45 + vec2(time * 0.04, time * 0.03));
  float foam = smoothstep(0.49, 0.78, sin((body + left * 0.45) * 15.708) * 0.5 + 0.5);
  float vignette = 1.0 - smoothstep(0.34, 0.98, length(p));
  vec3 deep = mix(uAtmoVoid, uAtmoFar, body * 0.78);
  vec3 current = mix(uAtmoNear, uAtmoGlow, left * 0.65 + right * 0.18);
  vec3 color = mix(deep, current, 0.42 + foam * 0.46);
  float alpha = (0.16 + foam * 0.56) * vignette * uIntensity * mix(0.6, 1.0, uQuality);
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`fluid-sim`,title:`Fluid Simulation`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:14,priority:14,exclusiveGroup:`background-ambient`}),uniforms:Object.freeze({uIntensity:Object.freeze({type:`float`,default:.24,min:0,max:.48,step:.01,description:`Fluid veil opacity.`}),uSpeed:Object.freeze({type:`float`,default:.26,min:0,max:1.25,step:.01,description:`Fixed-clock advection speed.`}),uViscosity:Object.freeze({type:`float`,default:.62,min:.15,max:1.4,step:.01,description:`Eddy smoothing and scale.`}),uTurbulence:Object.freeze({type:`float`,default:.7,min:0,max:1.5,step:.01,description:`Analytic curl displacement.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,detailScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,detailScale:.76,alphaCap:.8}),low:Object.freeze({renderScale:.5,detailScale:.5,alphaCap:.58})}),cost:Object.freeze({budgetUnits:Object.freeze({high:3,medium:1.9,low:.85}),gpuMilliseconds:Object.freeze({high:1.25,medium:.76,low:.4}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n){let r=1-.001**Math.max(0,Math.min(n,.25));return e+(t-e)*r}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=Object.keys(n.uniforms),u=Object.create(null),d=Object.create(null),f=Object.create(null);for(let e=0;e<l.length;e+=1){let t=l[e];u[t]=s.getUniformLocation(c.program,t),d[t]=r(o[t],n.uniforms[t]),f[t]=d[t]}let p=n.quality.medium,m=!0,h=!1;function g(e={}){for(let t=0;t<l.length;t+=1){let i=l[t];Object.prototype.hasOwnProperty.call(e,i)&&(d[i]=r(e[i],n.uniforms[i]))}}function _(e,t){p=n.quality[e]||n.quality.medium,m=!!t}function v(){}function y(e){for(let t=0;t<l.length;t+=1){let n=l[t];f[n]=i(f[n],d[n],e)}}function b(e){if(!(h||!m)){s.useProgram(c.program),a.bindEngineGlobals(c,e);for(let e=0;e<l.length;e+=1){let t=l[e],n=u[t];if(n===null)continue;let r=f[t];t===`uIntensity`&&(r*=p.alphaCap),t===`uTurbulence`&&(r*=p.detailScale),s.uniform1f(n,r)}s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3)}}function x(){h||(h=!0,a.disposeProgram(c))}return{setParameters:g,setQuality:_,resize:v,simulate:y,render:b,dispose:x}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-aurora-fluid-sim-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};