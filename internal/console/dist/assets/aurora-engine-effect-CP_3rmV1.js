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
uniform float uDensity;
uniform float uParallax;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(231.34, 512.77));
  p += dot(p, p + 33.33);
  return fract(p.x * p.y);
}

float starLayer(vec2 p, float scale, float time) {
  vec2 cell = floor(p * scale);
  vec2 local = fract(p * scale) - 0.5;
  float seed = hash21(cell);
  vec2 point = vec2(hash21(cell + 7.1), hash21(cell + 13.7)) - 0.5;
  float distanceToStar = length(local - point * 0.78);
  float radius = mix(0.025, 0.105, seed * seed);
  float sparkle = 0.72 + 0.28 * sin(time * (1.4 + seed * 2.2) + seed * 37.0);
  return (1.0 - smoothstep(radius * 0.45, radius, distanceToStar)) * sparkle;
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  float stars = 0.0;
  for (int layer = 0; layer < 3; layer += 1) {
    float depth = float(layer) + 1.0;
    float visibility = 1.0 - smoothstep(0.42, 1.0, float(layer) * (1.0 - uQuality));
    vec2 shift = vec2(time * (0.018 + depth * 0.014) + uParallax * depth * 0.055, -time * depth * 0.009);
    stars += starLayer(p + shift, (3.8 + depth * 3.4) * uDensity, time) * visibility * (0.62 + depth * 0.14);
  }
  float mist = smoothstep(1.06, 0.16, length(p + vec2(0.1, -0.08)));
  vec3 space = mix(uAtmoVoid, uAtmoFar, mist * 0.38);
  vec3 starColor = mix(uAtmoNear, uAtmoGlow, clamp(stars, 0.0, 1.0));
  vec3 color = mix(space, starColor, clamp(stars * 0.86, 0.0, 1.0));
  float alpha = min(0.78, (0.08 * mist + stars * 0.86) * uIntensity);
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`star-parallax`,title:`Star Parallax`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:15,priority:15,exclusiveGroup:`background-ambient`}),uniforms:Object.freeze({uIntensity:Object.freeze({type:`float`,default:.2,min:0,max:.46,step:.01,description:`Starfield opacity.`}),uSpeed:Object.freeze({type:`float`,default:.16,min:0,max:1,step:.01,description:`Fixed-clock layer drift.`}),uDensity:Object.freeze({type:`float`,default:.72,min:.2,max:1.5,step:.01,description:`Cell density for procedural stars.`}),uParallax:Object.freeze({type:`float`,default:0,min:-1,max:1,step:.01,description:`Caller-smoothed horizontal parallax offset.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,detailScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,detailScale:.73,alphaCap:.8}),low:Object.freeze({renderScale:.5,detailScale:.48,alphaCap:.58})}),cost:Object.freeze({budgetUnits:Object.freeze({high:2.2,medium:1.3,low:.55}),gpuMilliseconds:Object.freeze({high:.78,medium:.45,low:.24}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n){let r=1-.001**Math.max(0,Math.min(n,.25));return e+(t-e)*r}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=Object.keys(n.uniforms),u=Object.create(null),d=Object.create(null),f=Object.create(null);for(let e=0;e<l.length;e+=1){let t=l[e];u[t]=s.getUniformLocation(c.program,t),d[t]=r(o[t],n.uniforms[t]),f[t]=d[t]}let p=n.quality.medium,m=!0,h=!1;function g(e={}){for(let t=0;t<l.length;t+=1){let i=l[t];Object.prototype.hasOwnProperty.call(e,i)&&(d[i]=r(e[i],n.uniforms[i]))}}function _(e,t){p=n.quality[e]||n.quality.medium,m=!!t}function v(){}function y(e){for(let t=0;t<l.length;t+=1){let n=l[t];f[n]=i(f[n],d[n],e)}}function b(e){if(!(h||!m)){s.useProgram(c.program),a.bindEngineGlobals(c,e);for(let e=0;e<l.length;e+=1){let t=l[e],n=u[t];if(n===null)continue;let r=f[t];t===`uIntensity`&&(r*=p.alphaCap),t===`uDensity`&&(r*=p.detailScale),s.uniform1f(n,r)}s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3)}}function x(){h||(h=!0,a.disposeProgram(c))}return{setParameters:g,setQuality:_,resize:v,simulate:y,render:b,dispose:x}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-aurora-star-parallax-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};