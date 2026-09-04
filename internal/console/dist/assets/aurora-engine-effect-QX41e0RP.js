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
`,r=Object.freeze({durationMs:150,durationToken:`--pool-motion-fast`,easing:`cubic-bezier(0, 0, .2, 1)`,easingToken:`--pool-ease-enter`,reducedMotion:`skip-to-end`}),i=Object.freeze({schemaVersion:1,id:`page-slide`,title:`Page slide transition`,composition:Object.freeze({slot:`interaction`,blend:`alpha`,zIndex:47,priority:47,exclusiveGroup:`route-transition`}),transition:r,contract:Object.freeze({domContentOwner:!0,role:`directional-edge-accent`}),uniforms:Object.freeze({uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,description:`Slide progress.`}),uDirection:Object.freeze({type:`vec2`,default:[1,0],description:`Normalized slide direction.`}),uTint:Object.freeze({type:`vec3`,default:[.38,.75,.96],description:`Slide edge tint.`}),uIntensity:Object.freeze({type:`float`,default:.28,min:0,max:.58,description:`Edge accent opacity.`}),uSpeed:Object.freeze({type:`float`,default:.75,min:0,max:2,description:`Edge shimmer speed.`}),uDensity:Object.freeze({type:`float`,default:.54,min:.2,max:1,description:`Streak density.`}),uScale:Object.freeze({type:`float`,default:1,min:.65,max:1.8,description:`Streak scale.`}),uDistance:Object.freeze({type:`float`,default:.92,min:.2,max:1.6,description:`Normalized slide travel.`}),uSoftness:Object.freeze({type:`float`,default:.1,min:.02,max:.24,description:`Leading edge softness.`})}),quality:Object.freeze({high:Object.freeze({streakFactor:1,alphaCap:1}),medium:Object.freeze({streakFactor:.72,alphaCap:.78}),low:Object.freeze({streakFactor:.48,alphaCap:.52})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.98,medium:.64,low:.3}),gpuMilliseconds:Object.freeze({high:.21,medium:.14,low:.075}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function a(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function o(e,t,n){let r=Array.isArray(e)||ArrayBuffer.isView(e)?e:t;return Array.from({length:n},(e,n)=>{let i=Number(r[n]);return Number.isFinite(i)?i:Number(t[n])})}function s(e,t,n){let r=1-.001**(Math.max(0,n)/.15);return e+(t-e)*r}function c(){return typeof globalThis.matchMedia==`function`&&globalThis.matchMedia(`(prefers-reduced-motion: reduce)`).matches}function l(r,l={}){let u=r.gl,d=r.createProgram({vertexSource:t,fragmentSource:n,label:i.id}),f=Object.freeze({progress:u.getUniformLocation(d.program,`uProgress`),direction:u.getUniformLocation(d.program,`uDirection`),tint:u.getUniformLocation(d.program,`uTint`),intensity:u.getUniformLocation(d.program,`uIntensity`),speed:u.getUniformLocation(d.program,`uSpeed`),density:u.getUniformLocation(d.program,`uDensity`),scale:u.getUniformLocation(d.program,`uScale`),distance:u.getUniformLocation(d.program,`uDistance`),softness:u.getUniformLocation(d.program,`uSoftness`)}),p=i.uniforms,m=a(l.uProgress,p.uProgress),h=m,g=o(l.uDirection,p.uDirection.default,2),_=g.slice(),v=o(l.uTint,p.uTint.default,3).map(t=>e(t)),y=v.slice(),b=a(l.uIntensity,p.uIntensity),x=b,S=a(l.uSpeed,p.uSpeed),C=S,w=a(l.uDensity,p.uDensity),T=w,E=a(l.uScale,p.uScale),D=E,O=a(l.uDistance,p.uDistance),k=O,A=a(l.uSoftness,p.uSoftness),j=A,M=i.quality.medium,N=c(),P=!N,F=!1;function I(t={}){Object.prototype.hasOwnProperty.call(t,`uProgress`)&&(m=a(t.uProgress,p.uProgress)),Object.prototype.hasOwnProperty.call(t,`uDirection`)&&(g=o(t.uDirection,g,2)),Object.prototype.hasOwnProperty.call(t,`uTint`)&&(v=o(t.uTint,v,3).map(t=>e(t))),Object.prototype.hasOwnProperty.call(t,`uIntensity`)&&(b=a(t.uIntensity,p.uIntensity)),Object.prototype.hasOwnProperty.call(t,`uSpeed`)&&(S=a(t.uSpeed,p.uSpeed)),Object.prototype.hasOwnProperty.call(t,`uDensity`)&&(w=a(t.uDensity,p.uDensity)),Object.prototype.hasOwnProperty.call(t,`uScale`)&&(E=a(t.uScale,p.uScale)),Object.prototype.hasOwnProperty.call(t,`uDistance`)&&(O=a(t.uDistance,p.uDistance)),Object.prototype.hasOwnProperty.call(t,`uSoftness`)&&(A=a(t.uSoftness,p.uSoftness))}function L(e,t){M=i.quality[e]||i.quality.medium,P=!!t&&!N}function R(){}function z(e){if(N){h=1;return}h=s(h,m,e);for(let t=0;t<2;t+=1)_[t]=s(_[t],g[t],e);for(let t=0;t<3;t+=1)y[t]=s(y[t],v[t],e);x=s(x,b,e),C=s(C,S,e),T=s(T,w,e),D=s(D,E,e),k=s(k,O,e),j=s(j,A,e)}function B(t){F||!P||h<=.001||h>=.999||(u.useProgram(d.program),r.bindEngineGlobals(d,t),f.progress!==null&&u.uniform1f(f.progress,e(h)),f.direction!==null&&u.uniform2fv(f.direction,_),f.tint!==null&&u.uniform3fv(f.tint,y),f.intensity!==null&&u.uniform1f(f.intensity,x*M.alphaCap),f.speed!==null&&u.uniform1f(f.speed,C),f.density!==null&&u.uniform1f(f.density,T*M.streakFactor),f.scale!==null&&u.uniform1f(f.scale,D),f.distance!==null&&u.uniform1f(f.distance,k),f.softness!==null&&u.uniform1f(f.softness,j),u.bindVertexArray(r.fullscreenVao),u.drawArrays(u.TRIANGLES,0,3))}function V(){F||(F=!0,r.disposeProgram(d))}return{setParameters:I,setQuality:L,resize:R,simulate:z,render:B,dispose:V}}function u(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-page-slide-fallback`,r=e.getAttribute(n);e.setAttribute(n,String(t.reason||`dom-page-slide`));let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{u as applyDomFallback,l as createEffect,i as manifest,r as transition};