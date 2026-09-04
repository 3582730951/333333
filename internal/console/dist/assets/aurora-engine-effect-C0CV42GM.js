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
uniform vec2 uOrigin;
out vec4 fragColor;

void main() {
  vec2 p = vUv - uOrigin;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float radius = length(p);
  float travellingRadius = uProgress * 1.28 * uScale;
  float band = abs(radius - travellingRadius);
  float width = mix(0.035, 0.009, uDensity) / max(uScale, 0.01);
  float mainRing = 1.0 - smoothstep(width, width * 2.2, band);
  float secondaryRadius = max(0.0, travellingRadius - mix(0.08, 0.22, uDensity));
  float secondary = 1.0 - smoothstep(width * 1.4, width * 3.2, abs(radius - secondaryRadius));
  float shimmer = 0.72 + 0.28 * sin(radius * mix(28.0, 76.0, uDensity) - uTime * uSpeed * 4.0);
  float fade = 1.0 - smoothstep(0.78, 1.12, radius);
  vec3 color = mix(uAtmoNear, uTint, 0.82) + uAtmoGlow * 0.2;
  float alpha = (mainRing + secondary * 0.42) * shimmer * fade * uIntensity * mix(0.55, 1.0, uQuality);
  fragColor = vec4(color, alpha);
}
`,r=Object.freeze({durationMs:300,durationToken:`--pool-motion-slow`,easing:`cubic-bezier(.2, 0, 0, 1)`,easingToken:`--pool-ease-emphasized`,reducedMotion:`skip-to-end`}),i=Object.freeze({schemaVersion:1,id:`ripple`,title:`Ripple transition`,composition:Object.freeze({slot:`interaction`,blend:`additive`,zIndex:41,priority:41,exclusiveGroup:`route-transition`}),transition:r,uniforms:Object.freeze({uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,description:`Transition progress from origin to settled state.`}),uTint:Object.freeze({type:`vec3`,default:[.42,.84,.94],description:`Ripple light color.`}),uIntensity:Object.freeze({type:`float`,default:.32,min:0,max:.7,description:`Ripple opacity.`}),uSpeed:Object.freeze({type:`float`,default:1,min:0,max:2,description:`Ring shimmer only; does not change the 300ms duration.`}),uDensity:Object.freeze({type:`float`,default:.55,min:.2,max:1,description:`Concentric-ring density.`}),uScale:Object.freeze({type:`float`,default:1,min:.6,max:1.8,description:`Ripple radius scale.`}),uOrigin:Object.freeze({type:`vec2`,default:[.5,.5],description:`Normalized triggering-point origin.`})}),quality:Object.freeze({high:Object.freeze({ringCount:1,alphaCap:1}),medium:Object.freeze({ringCount:.76,alphaCap:.76}),low:Object.freeze({ringCount:.52,alphaCap:.54})}),cost:Object.freeze({budgetUnits:Object.freeze({high:1.1,medium:.75,low:.38}),gpuMilliseconds:Object.freeze({high:.24,medium:.16,low:.09}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function a(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function o(t,n,r,i){let a=Array.isArray(t)||ArrayBuffer.isView(t)?t:n.default;for(let t=0;t<i;t+=1)r[t]=e(Number(a[t]))}function s(e,t,n){let i=1-.001**(Math.max(0,n)/(r.durationMs/1e3));return e+(t-e)*i}function c(){return typeof globalThis.matchMedia==`function`&&globalThis.matchMedia(`(prefers-reduced-motion: reduce)`).matches}function l(r,l={}){let u=r.gl,d=r.createProgram({vertexSource:t,fragmentSource:n,label:i.id}),f=Object.freeze({progress:u.getUniformLocation(d.program,`uProgress`),tint:u.getUniformLocation(d.program,`uTint`),intensity:u.getUniformLocation(d.program,`uIntensity`),speed:u.getUniformLocation(d.program,`uSpeed`),density:u.getUniformLocation(d.program,`uDensity`),scale:u.getUniformLocation(d.program,`uScale`),origin:u.getUniformLocation(d.program,`uOrigin`)}),p=i.uniforms,m=a(l.uProgress,p.uProgress),h=m,g=a(l.uIntensity,p.uIntensity),_=g,v=a(l.uSpeed,p.uSpeed),y=v,b=a(l.uDensity,p.uDensity),x=b,S=a(l.uScale,p.uScale),C=S,w=[0,0,0],T=[0,0,0],E=[0,0],D=[0,0];o(l.uTint,p.uTint,w,3),o(l.uTint,p.uTint,T,3),o(l.uOrigin,p.uOrigin,E,2),o(l.uOrigin,p.uOrigin,D,2);let O=i.quality.medium,k=c(),A=!k,j=!1;function M(e={}){Object.prototype.hasOwnProperty.call(e,`uProgress`)&&(m=a(e.uProgress,p.uProgress)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(g=a(e.uIntensity,p.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(v=a(e.uSpeed,p.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uDensity`)&&(b=a(e.uDensity,p.uDensity)),Object.prototype.hasOwnProperty.call(e,`uScale`)&&(S=a(e.uScale,p.uScale)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&o(e.uTint,p.uTint,w,3),Object.prototype.hasOwnProperty.call(e,`uOrigin`)&&o(e.uOrigin,p.uOrigin,E,2)}function N(e,t){O=i.quality[e]||i.quality.medium,A=!!t&&!k}function P(){}function F(e){if(k){h=1;return}h=s(h,m,e),_=s(_,g,e),y=s(y,v,e),x=s(x,b,e),C=s(C,S,e);for(let t=0;t<3;t+=1)T[t]=s(T[t],w[t],e);for(let t=0;t<2;t+=1)D[t]=s(D[t],E[t],e)}function I(t){j||!A||h<=.001||h>=.999||(u.useProgram(d.program),r.bindEngineGlobals(d,t),f.progress!==null&&u.uniform1f(f.progress,e(h)),f.tint!==null&&u.uniform3fv(f.tint,T),f.intensity!==null&&u.uniform1f(f.intensity,_*O.alphaCap),f.speed!==null&&u.uniform1f(f.speed,y),f.density!==null&&u.uniform1f(f.density,x*O.ringCount),f.scale!==null&&u.uniform1f(f.scale,C),f.origin!==null&&u.uniform2fv(f.origin,D),u.bindVertexArray(r.fullscreenVao),u.drawArrays(u.TRIANGLES,0,3))}function L(){j||(j=!0,r.disposeProgram(d))}return{setParameters:M,setQuality:N,resize:P,simulate:F,render:I,dispose:L}}function u(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-ripple-fallback`,r=`data-aurora-ripple-reason`,i=e.getAttribute(n),a=e.getAttribute(r);return e.setAttribute(n,`true`),e.setAttribute(r,String(t.reason||`transition-skipped`)),()=>{i===null?e.removeAttribute(n):e.setAttribute(n,i),a===null?e.removeAttribute(r):e.setAttribute(r,a)}}export{u as applyDomFallback,l as createEffect,i as manifest,r as transition};