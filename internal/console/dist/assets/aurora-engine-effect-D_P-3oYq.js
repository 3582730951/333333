import{n as e}from"./aurora-engine-contracts-CjO_kDw4.js";var t=`#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,n=`#version 300 es
precision mediump float;

in vec2 vUv;
uniform float uTime;
uniform vec2 uResolution;
uniform float uQuality;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoGlow;
uniform float uProgress;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(123.34, 456.21));
  p += dot(p, p + 45.32);
  return fract(p.x * p.y);
}

float valueNoise(vec2 p) {
  vec2 cell = floor(p);
  vec2 local = fract(p);
  local = local * local * (3.0 - 2.0 * local);
  float a = hash21(cell);
  float b = hash21(cell + vec2(1.0, 0.0));
  float c = hash21(cell + vec2(0.0, 1.0));
  float d = hash21(cell + vec2(1.0, 1.0));
  return mix(mix(a, b, local.x), mix(c, d, local.x), local.y);
}

void main() {
  vec2 aspectUv = vUv;
  aspectUv.x *= uResolution.x / max(uResolution.y, 1.0);
  float cells = mix(14.0, 72.0, uDensity) * uScale * mix(0.6, 1.0, uQuality);
  float grain = valueNoise(aspectUv * cells + vec2(uTime * uSpeed * 0.04, 0.0));
  float threshold = mix(-0.08, 1.08, uProgress);
  float feather = mix(0.11, 0.035, uDensity);
  float revealed = smoothstep(threshold - feather, threshold + feather, grain);
  float edge = 1.0 - smoothstep(0.0, feather * 2.5, abs(grain - threshold));
  float vignette = smoothstep(1.05, 0.18, length(vUv - 0.5));
  vec3 color = mix(uAtmoNear, uTint, 0.72) + uAtmoGlow * edge * 0.18;
  float alpha = (revealed * 0.22 + edge * 0.78) * vignette * uIntensity;
  fragColor = vec4(color, alpha);
}
`,r=Object.freeze({durationMs:200,durationToken:`--pool-motion-normal`,easing:`cubic-bezier(.2, .8, .2, 1)`,easingToken:`--pool-ease-standard`,reducedMotion:`skip-to-end`}),i=Object.freeze({schemaVersion:1,id:`dissolve`,title:`Dissolve transition`,composition:Object.freeze({slot:`interaction`,blend:`alpha`,zIndex:40,priority:40,exclusiveGroup:`route-transition`}),transition:r,uniforms:Object.freeze({uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,description:`Transition progress from start to end.`}),uTint:Object.freeze({type:`vec3`,default:[.52,.77,1],description:`Dissolve edge color.`}),uIntensity:Object.freeze({type:`float`,default:.34,min:0,max:.72,description:`Layer opacity.`}),uSpeed:Object.freeze({type:`float`,default:1,min:0,max:2,description:`Pattern drift only; does not change the 200ms duration.`}),uDensity:Object.freeze({type:`float`,default:.58,min:.2,max:1,description:`Noise-cell density.`}),uScale:Object.freeze({type:`float`,default:1,min:.6,max:1.8,description:`Noise scale.`})}),quality:Object.freeze({high:Object.freeze({noiseScale:1,alphaCap:1}),medium:Object.freeze({noiseScale:.78,alphaCap:.78}),low:Object.freeze({noiseScale:.54,alphaCap:.56})}),cost:Object.freeze({budgetUnits:Object.freeze({high:1.2,medium:.8,low:.4}),gpuMilliseconds:Object.freeze({high:.28,medium:.18,low:.1}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function a(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function o(t,n,r){let i=Array.isArray(t)||ArrayBuffer.isView(t)?t:n.default;r[0]=e(Number(i[0])),r[1]=e(Number(i[1])),r[2]=e(Number(i[2]))}function s(e,t,n){let i=r.durationMs/1e3,a=1-.001**(Math.max(0,n)/i);return e+(t-e)*a}function c(){return typeof globalThis.matchMedia==`function`&&globalThis.matchMedia(`(prefers-reduced-motion: reduce)`).matches}function l(r,l={}){let u=r.gl,d=r.createProgram({vertexSource:t,fragmentSource:n,label:i.id}),f=Object.freeze({progress:u.getUniformLocation(d.program,`uProgress`),tint:u.getUniformLocation(d.program,`uTint`),intensity:u.getUniformLocation(d.program,`uIntensity`),speed:u.getUniformLocation(d.program,`uSpeed`),density:u.getUniformLocation(d.program,`uDensity`),scale:u.getUniformLocation(d.program,`uScale`)}),p=i.uniforms,m=a(l.uProgress,p.uProgress),h=m,g=a(l.uIntensity,p.uIntensity),_=g,v=a(l.uSpeed,p.uSpeed),y=v,b=a(l.uDensity,p.uDensity),x=b,S=a(l.uScale,p.uScale),C=S,w=[0,0,0],T=[0,0,0];o(l.uTint,p.uTint,w),o(l.uTint,p.uTint,T);let E=i.quality.medium,D=c(),O=!D,k=!1;function A(e={}){Object.prototype.hasOwnProperty.call(e,`uProgress`)&&(m=a(e.uProgress,p.uProgress)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(g=a(e.uIntensity,p.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(v=a(e.uSpeed,p.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uDensity`)&&(b=a(e.uDensity,p.uDensity)),Object.prototype.hasOwnProperty.call(e,`uScale`)&&(S=a(e.uScale,p.uScale)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&o(e.uTint,p.uTint,w)}function j(e,t){E=i.quality[e]||i.quality.medium,O=!!t&&!D}function M(){}function N(e){if(D){h=1;return}h=s(h,m,e),_=s(_,g,e),y=s(y,v,e),x=s(x,b,e),C=s(C,S,e),T[0]=s(T[0],w[0],e),T[1]=s(T[1],w[1],e),T[2]=s(T[2],w[2],e)}function P(t){k||!O||h<=.001||h>=.999||(u.useProgram(d.program),r.bindEngineGlobals(d,t),f.progress!==null&&u.uniform1f(f.progress,e(h)),f.tint!==null&&u.uniform3fv(f.tint,T),f.intensity!==null&&u.uniform1f(f.intensity,_*E.alphaCap),f.speed!==null&&u.uniform1f(f.speed,y),f.density!==null&&u.uniform1f(f.density,x*E.noiseScale),f.scale!==null&&u.uniform1f(f.scale,C),u.bindVertexArray(r.fullscreenVao),u.drawArrays(u.TRIANGLES,0,3))}function F(){k||(k=!0,r.disposeProgram(d))}return{setParameters:A,setQuality:j,resize:M,simulate:N,render:P,dispose:F}}function u(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-dissolve-fallback`,r=`data-aurora-dissolve-reason`,i=e.getAttribute(n),a=e.getAttribute(r);return e.setAttribute(n,`true`),e.setAttribute(r,String(t.reason||`transition-skipped`)),()=>{i===null?e.removeAttribute(n):e.setAttribute(n,i),a===null?e.removeAttribute(r):e.setAttribute(r,a)}}export{u as applyDomFallback,l as createEffect,i as manifest,r as transition};