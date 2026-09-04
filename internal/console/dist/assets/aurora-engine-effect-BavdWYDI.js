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
uniform float uRoughness;
uniform float uDistortion;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(91.7, 173.3));
  p += dot(p, p + 27.1);
  return fract(p.x * p.y);
}

float noise(vec2 p) {
  vec2 cell = floor(p);
  vec2 local = fract(p);
  vec2 curve = local * local * (3.0 - 2.0 * local);
  float a = hash21(cell);
  float b = hash21(cell + vec2(1.0, 0.0));
  float c = hash21(cell + vec2(0.0, 1.0));
  float d = hash21(cell + vec2(1.0, 1.0));
  return mix(mix(a, b, curve.x), mix(c, d, curve.x), curve.y);
}

float layered(vec2 p) {
  float value = 0.0;
  float weight = 0.62;
  for (int octave = 0; octave < 3; octave += 1) {
    value += noise(p) * weight;
    p = mat2(1.56, -1.07, 1.07, 1.56) * p + vec2(2.2, -1.6);
    weight *= 0.48;
  }
  return value;
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  vec2 domain = p * 2.8;
  float base = layered(domain + vec2(time * 0.12, -time * 0.08));
  float detail = layered(domain * 1.7 + vec2(-time * 0.07, time * 0.11));
  vec2 normal = vec2(detail - 0.45, base - 0.45) * uDistortion;
  float reflection = layered(domain + normal * 2.6 + vec2(time * 0.03));
  float highlight = pow(max(0.0, 1.0 - abs(reflection * 2.0 - 1.0)), mix(2.0, 7.0, uRoughness));
  float edge = 1.0 - smoothstep(0.34, 1.0, length(p));
  vec3 dark = mix(uAtmoVoid, uAtmoFar, base * 0.74);
  vec3 metal = mix(uAtmoNear, uAtmoGlow, highlight * 0.78 + detail * 0.18);
  vec3 color = mix(dark, metal, 0.34 + highlight * 0.5);
  float alpha = (0.11 + highlight * 0.65) * edge * uIntensity;
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`liquid-metal`,title:`Liquid Metal`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:17,priority:17,exclusiveGroup:`background-ambient`}),uniforms:Object.freeze({uIntensity:Object.freeze({type:`float`,default:.2,min:0,max:.45,step:.01,description:`Reflective surface opacity.`}),uSpeed:Object.freeze({type:`float`,default:.3,min:0,max:1.4,step:.01,description:`Fixed-clock liquid motion.`}),uRoughness:Object.freeze({type:`float`,default:.38,min:.08,max:.9,step:.01,description:`Reflection blur.`}),uDistortion:Object.freeze({type:`float`,default:.42,min:0,max:1.2,step:.01,description:`Surface normal distortion.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,detailScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,detailScale:.74,alphaCap:.8}),low:Object.freeze({renderScale:.5,detailScale:.5,alphaCap:.58})}),cost:Object.freeze({budgetUnits:Object.freeze({high:2.6,medium:1.65,low:.72}),gpuMilliseconds:Object.freeze({high:1.05,medium:.64,low:.34}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n){let r=1-.001**Math.max(0,Math.min(n,.25));return e+(t-e)*r}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=Object.keys(n.uniforms),u=Object.create(null),d=Object.create(null),f=Object.create(null);for(let e=0;e<l.length;e+=1){let t=l[e];u[t]=s.getUniformLocation(c.program,t),d[t]=r(o[t],n.uniforms[t]),f[t]=d[t]}let p=n.quality.medium,m=!0,h=!1;function g(e={}){for(let t=0;t<l.length;t+=1){let i=l[t];Object.prototype.hasOwnProperty.call(e,i)&&(d[i]=r(e[i],n.uniforms[i]))}}function _(e,t){p=n.quality[e]||n.quality.medium,m=!!t}function v(){}function y(e){for(let t=0;t<l.length;t+=1){let n=l[t];f[n]=i(f[n],d[n],e)}}function b(e){if(!(h||!m)){s.useProgram(c.program),a.bindEngineGlobals(c,e);for(let e=0;e<l.length;e+=1){let t=l[e],n=u[t];if(n===null)continue;let r=f[t];t===`uIntensity`&&(r*=p.alphaCap),t===`uDistortion`&&(r*=p.detailScale),s.uniform1f(n,r)}s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3)}}function x(){h||(h=!0,a.disposeProgram(c))}return{setParameters:g,setQuality:_,resize:v,simulate:y,render:b,dispose:x}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-aurora-liquid-metal-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};