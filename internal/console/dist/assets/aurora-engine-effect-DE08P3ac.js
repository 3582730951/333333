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
uniform float uOrientation;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
uniform float uSoftness;
out vec4 fragColor;

void main() {
  vec2 p = vUv;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float axis = mix(vUv.y, vUv.x, step(0.5, uOrientation));
  float count = mix(7.0, 34.0, uDensity) * uScale;
  float slatPosition = fract(axis * count);
  float slatIndex = floor(axis * count);
  float stagger = fract(slatIndex * 0.618);
  float threshold = uProgress + (stagger - 0.5) * 0.18;
  float reveal = smoothstep(threshold - uSoftness, threshold + uSoftness, slatPosition);
  float edge = 1.0 - smoothstep(0.0, uSoftness * 2.4, abs(slatPosition - threshold));
  float shimmer = 0.86 + 0.14 * sin(slatIndex * 1.7 + uTime * uSpeed * 3.0);
  float vignette = 1.0 - smoothstep(0.25, 0.78, length(p - vec2(0.5, 0.5)));
  vec3 color = mix(uAtmoNear, uTint, 0.75) + uAtmoGlow * edge * 0.16;
  float alpha = (1.0 - reveal) * (0.22 + edge * 0.78) * shimmer * vignette * uIntensity;
  fragColor = vec4(color, alpha);
}
`,r=Object.freeze({durationMs:150,durationToken:`--pool-motion-fast`,easing:`cubic-bezier(.2, .8, .2, 1)`,easingToken:`--pool-ease-standard`,reducedMotion:`skip-to-end`}),i=Object.freeze({schemaVersion:1,id:`blinds`,title:`Blinds transition`,composition:Object.freeze({slot:`interaction`,blend:`alpha`,zIndex:42,priority:42,exclusiveGroup:`route-transition`}),transition:r,uniforms:Object.freeze({uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,description:`Slat reveal progress.`}),uOrientation:Object.freeze({type:`float`,default:0,min:0,max:1,description:`0 horizontal slats, 1 vertical slats.`}),uTint:Object.freeze({type:`vec3`,default:[.3,.64,.9],description:`Slat edge tint.`}),uIntensity:Object.freeze({type:`float`,default:.28,min:0,max:.6,description:`Slat opacity.`}),uSpeed:Object.freeze({type:`float`,default:1,min:0,max:2,description:`Subtle slat shimmer speed.`}),uDensity:Object.freeze({type:`float`,default:.6,min:.2,max:1,description:`Number of slats.`}),uScale:Object.freeze({type:`float`,default:1,min:.65,max:1.7,description:`Slat width scale.`}),uSoftness:Object.freeze({type:`float`,default:.09,min:.02,max:.22,description:`Reveal edge softness.`})}),quality:Object.freeze({high:Object.freeze({slatFactor:1,alphaCap:1}),medium:Object.freeze({slatFactor:.72,alphaCap:.78}),low:Object.freeze({slatFactor:.48,alphaCap:.54})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.95,medium:.62,low:.3}),gpuMilliseconds:Object.freeze({high:.2,medium:.13,low:.07}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function a(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function o(e,t,n){let i=1-.001**(Math.max(0,n)/(r.durationMs/1e3));return e+(t-e)*i}function s(){return typeof globalThis.matchMedia==`function`&&globalThis.matchMedia(`(prefers-reduced-motion: reduce)`).matches}function c(r,c={}){let l=r.gl,u=r.createProgram({vertexSource:t,fragmentSource:n,label:i.id}),d=Object.freeze({progress:l.getUniformLocation(u.program,`uProgress`),orientation:l.getUniformLocation(u.program,`uOrientation`),tint:l.getUniformLocation(u.program,`uTint`),intensity:l.getUniformLocation(u.program,`uIntensity`),speed:l.getUniformLocation(u.program,`uSpeed`),density:l.getUniformLocation(u.program,`uDensity`),scale:l.getUniformLocation(u.program,`uScale`),softness:l.getUniformLocation(u.program,`uSoftness`)}),f=i.uniforms,p=(t,n)=>{let r=Array.isArray(t)||ArrayBuffer.isView(t)?t:n;return[e(r[0]),e(r[1]),e(r[2])]},m=a(c.uProgress,f.uProgress),h=m,g=a(c.uOrientation,f.uOrientation),_=g,v=p(c.uTint,f.uTint.default),y=v.slice(),b=a(c.uIntensity,f.uIntensity),x=b,S=a(c.uSpeed,f.uSpeed),C=S,w=a(c.uDensity,f.uDensity),T=w,E=a(c.uScale,f.uScale),D=E,O=a(c.uSoftness,f.uSoftness),k=O,A=i.quality.medium,j=s(),M=!j,N=!1;function P(e={}){Object.prototype.hasOwnProperty.call(e,`uProgress`)&&(m=a(e.uProgress,f.uProgress)),Object.prototype.hasOwnProperty.call(e,`uOrientation`)&&(g=a(e.uOrientation,f.uOrientation)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(v=p(e.uTint,v)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(b=a(e.uIntensity,f.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(S=a(e.uSpeed,f.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uDensity`)&&(w=a(e.uDensity,f.uDensity)),Object.prototype.hasOwnProperty.call(e,`uScale`)&&(E=a(e.uScale,f.uScale)),Object.prototype.hasOwnProperty.call(e,`uSoftness`)&&(O=a(e.uSoftness,f.uSoftness))}function F(e,t){A=i.quality[e]||i.quality.medium,M=!!t&&!j}function I(){}function L(e){if(j){h=1;return}h=o(h,m,e),_=o(_,g,e),x=o(x,b,e),C=o(C,S,e),T=o(T,w,e),D=o(D,E,e),k=o(k,O,e);for(let t=0;t<3;t+=1)y[t]=o(y[t],v[t],e)}function R(t){N||!M||h<=.001||h>=.999||(l.useProgram(u.program),r.bindEngineGlobals(u,t),d.progress!==null&&l.uniform1f(d.progress,e(h)),d.orientation!==null&&l.uniform1f(d.orientation,_),d.tint!==null&&l.uniform3fv(d.tint,y),d.intensity!==null&&l.uniform1f(d.intensity,x*A.alphaCap),d.speed!==null&&l.uniform1f(d.speed,C),d.density!==null&&l.uniform1f(d.density,T*A.slatFactor),d.scale!==null&&l.uniform1f(d.scale,D),d.softness!==null&&l.uniform1f(d.softness,k),l.bindVertexArray(r.fullscreenVao),l.drawArrays(l.TRIANGLES,0,3))}function z(){N||(N=!0,r.disposeProgram(u))}return{setParameters:P,setQuality:F,resize:I,simulate:L,render:R,dispose:z}}function l(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-blinds-fallback`,r=`data-aurora-blinds-reason`,i=e.getAttribute(n),a=e.getAttribute(r);e.setAttribute(n,`true`),e.setAttribute(r,String(t.reason||`transition-skipped`));let o=!1;return()=>{o||(o=!0,i===null?e.removeAttribute(n):e.setAttribute(n,i),a===null?e.removeAttribute(r):e.setAttribute(r,a))}}export{l as applyDomFallback,c as createEffect,i as manifest,r as transition};