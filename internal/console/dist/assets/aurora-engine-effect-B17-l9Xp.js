import"./aurora-engine-contracts-CjO_kDw4.js";var e=`#version 300 es
precision mediump float;

uniform vec4 uElementRect;
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  vec2 position = uElementRect.xy + corner * uElementRect.zw;
  gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}
`,t=`#version 300 es
precision mediump float;

in vec2 vUv;
uniform float uTime;
uniform vec2 uResolution;
uniform float uPixelRatio;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uDensity;
uniform float uGrainSize;
uniform float uSpeed;
uniform float uCornerRadius;
uniform float uGrainDetail;
uniform float uAlphaCap;

out vec4 fragColor;

float hash12(vec2 point) {
  return fract(sin(dot(point, vec2(127.1, 311.7))) * 437.58);
}

float roundedBox(vec2 point, vec2 halfSize, float radius) {
  vec2 q = abs(point) - halfSize + radius;
  return length(max(q, 0.0)) + min(max(q.x, q.y), 0.0) - radius;
}

void main() {
  float aspect = uElementRect.z / max(uElementRect.w, 0.0001);
  vec2 point = vUv - 0.5;
  point.x *= aspect;
  float radius = min(uCornerRadius, min(0.49, 0.49 * aspect));
  float shape = 1.0 - smoothstep(0.0, 0.012, roundedBox(point, vec2(0.5 * aspect, 0.5), radius));

  // Work in physical pixels. Multiplying the CSS-sized grain cell by DPR
  // keeps the grain crisp and at a stable apparent size on dense displays.
  vec2 physicalSize = max(uElementRect.zw * uResolution, vec2(1.0));
  vec2 pixel = vUv * physicalSize;
  float dpr = max(uPixelRatio, 1.0);
  float cell = max(0.5, uGrainSize * dpr);
  vec2 lattice = floor(pixel / cell * max(uDensity, 0.1));
  float coarse = hash12(lattice + floor(uTime * uSpeed * 5.0));
  float fine = hash12(floor(pixel / dpr));
  float grain = mix(coarse, fine, uGrainDetail);
  vec3 base = mix(uAtmoNear, uAtmoFar, vUv.y);
  vec3 color = mix(base, uTint, 0.2 + grain * 0.5);
  float alpha = shape * uIntensity * uAlphaCap * (0.72 + grain * 0.28);
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`noise-grain`,title:`Noise Grain`,composition:Object.freeze({slot:`foreground`,blend:`alpha`,zIndex:40,priority:40,exclusiveGroup:`material-surface`}),uniforms:Object.freeze({uElementRect:Object.freeze({type:`vec4`,default:[.25,.25,.5,.5],description:`Normalized [left, bottom, width, height] target rect.`}),uTint:Object.freeze({type:`vec3`,default:[.8,.9,1],description:`Grain tint mixed with the Aurora near/far palette.`}),uIntensity:Object.freeze({type:`float`,default:.16,min:0,max:.5,step:.01,description:`Surface grain opacity.`}),uDensity:Object.freeze({type:`float`,default:.9,min:.1,max:2,step:.01,description:`Procedural grain cell density.`}),uGrainSize:Object.freeze({type:`float`,default:1.2,min:.25,max:4,step:.05,description:`Grain size in CSS pixels before DPR scaling.`}),uSpeed:Object.freeze({type:`float`,default:.2,min:0,max:2,step:.01,description:`Fixed-clock grain evolution speed.`}),uCornerRadius:Object.freeze({type:`float`,default:.08,min:0,max:.3,step:.01,description:`Rounded surface mask.`})}),quality:Object.freeze({high:Object.freeze({grainDetail:.75,alphaCap:1}),medium:Object.freeze({grainDetail:.48,alphaCap:.74}),low:Object.freeze({grainDetail:.2,alphaCap:.48})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.35,medium:.22,low:.09}),gpuMilliseconds:Object.freeze({high:.1,medium:.065,low:.035}),fill:`partial`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r,i){let a=Number(e&&e[t]);return Number.isFinite(a)?Math.min(i,Math.max(r,a)):n[t]}function a(e,t){return[i(e,0,t,-.5,1.5),i(e,1,t,-.5,1.5),i(e,2,t,.001,2),i(e,3,t,.001,2)]}function o(e,t){return[i(e,0,t,0,1),i(e,1,t,0,1),i(e,2,t,0,1)]}function s(e,t,n){return e+(t-e)*(1-1e-4**Math.max(0,n))}function c(i,c={}){let l=i.gl,u=i.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),d=Object.freeze({rect:l.getUniformLocation(u.program,`uElementRect`),tint:l.getUniformLocation(u.program,`uTint`),intensity:l.getUniformLocation(u.program,`uIntensity`),density:l.getUniformLocation(u.program,`uDensity`),grainSize:l.getUniformLocation(u.program,`uGrainSize`),speed:l.getUniformLocation(u.program,`uSpeed`),cornerRadius:l.getUniformLocation(u.program,`uCornerRadius`),grainDetail:l.getUniformLocation(u.program,`uGrainDetail`),alphaCap:l.getUniformLocation(u.program,`uAlphaCap`)}),f=n.uniforms,p=a(c.uElementRect,f.uElementRect.default),m=p.slice(),h=o(c.uTint,f.uTint.default),g=h.slice(),_=r(c.uIntensity,f.uIntensity),v=_,y=r(c.uDensity,f.uDensity),b=y,x=r(c.uGrainSize,f.uGrainSize),S=x,C=r(c.uSpeed,f.uSpeed),w=C,T=r(c.uCornerRadius,f.uCornerRadius),E=T,D=n.quality.medium,O=!0,k=!1;function A(e={}){Object.prototype.hasOwnProperty.call(e,`uElementRect`)&&(p=a(e.uElementRect,p)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(h=o(e.uTint,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,f.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uDensity`)&&(y=r(e.uDensity,f.uDensity)),Object.prototype.hasOwnProperty.call(e,`uGrainSize`)&&(x=r(e.uGrainSize,f.uGrainSize)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(C=r(e.uSpeed,f.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uCornerRadius`)&&(T=r(e.uCornerRadius,f.uCornerRadius))}function j(e,t){D=n.quality[e]||n.quality.medium,O=!!t}function M(){}function N(e){for(let t=0;t<4;t+=1)m[t]=s(m[t],p[t],e);for(let t=0;t<3;t+=1)g[t]=s(g[t],h[t],e);v=s(v,_,e),b=s(b,y,e),S=s(S,x,e),w=s(w,C,e),E=s(E,T,e)}function P(e){k||!O||(l.useProgram(u.program),i.bindEngineGlobals(u,e),d.rect!==null&&l.uniform4f(d.rect,...m),d.tint!==null&&l.uniform3f(d.tint,...g),d.intensity!==null&&l.uniform1f(d.intensity,v),d.density!==null&&l.uniform1f(d.density,b),d.grainSize!==null&&l.uniform1f(d.grainSize,S),d.speed!==null&&l.uniform1f(d.speed,w),d.cornerRadius!==null&&l.uniform1f(d.cornerRadius,E),d.grainDetail!==null&&l.uniform1f(d.grainDetail,D.grainDetail),d.alphaCap!==null&&l.uniform1f(d.alphaCap,D.alphaCap),l.bindVertexArray(i.fullscreenVao),l.drawArrays(l.TRIANGLES,0,3))}function F(){k||(k=!0,i.disposeProgram(u))}return{setParameters:A,setQuality:j,resize:M,simulate:N,render:P,dispose:F}}function l(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-noise-grain-fallback`,r=e.getAttribute(n);e.setAttribute(n,t.state||`active`);let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{l as applyDomFallback,c as createEffect,n as manifest};