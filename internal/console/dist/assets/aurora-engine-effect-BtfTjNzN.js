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
uniform vec2 uOrigin;
uniform vec4 uFromRect;
uniform vec4 uToRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
uniform float uCornerRadius;
out vec4 fragColor;

float roundedBox(vec2 point, vec2 halfSize, float radius) {
  vec2 q = abs(point) - halfSize + radius;
  return length(max(q, vec2(0.0))) + min(max(q.x, q.y), 0.0) - radius;
}

void main() {
  vec4 rect = mix(uFromRect, uToRect, uProgress);
  vec2 centre = rect.xy + rect.zw * 0.5;
  centre = mix(uOrigin, centre, smoothstep(0.0, 1.0, uProgress));
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  vec2 point = (vUv - centre) * aspect;
  vec2 halfSize = max(rect.zw * 0.5 * aspect, vec2(0.001));
  float radius = min(min(halfSize.x, halfSize.y) * uCornerRadius * 2.0, 0.45);
  float distanceToBox = roundedBox(point, halfSize, radius);
  float width = mix(0.003, 0.013, uDensity) * uScale;
  float outline = 1.0 - smoothstep(width, width * 2.3, abs(distanceToBox));
  float interior = 1.0 - smoothstep(0.0, width * 2.0, distanceToBox);
  float shimmer = 0.86 + 0.14 * sin((point.x - point.y) * mix(18.0, 60.0, uDensity) + uTime * uSpeed * 3.0);
  vec3 colour = mix(uTint, uAtmoGlow, 0.2);
  float alpha = (outline * 0.84 + interior * 0.06) * shimmer * uIntensity * (0.58 + 0.42 * uQuality);
  fragColor = vec4(colour, alpha);
}
`,r=Object.freeze({durationMs:300,durationToken:`--pool-motion-slow`,easing:`cubic-bezier(0, 0, .2, 1)`,easingToken:`--pool-ease-enter`,reducedMotion:`skip-to-end`}),i=Object.freeze({schemaVersion:1,id:`modal-expand`,title:`Modal expand transition`,composition:Object.freeze({slot:`foreground`,blend:`alpha`,zIndex:46,priority:46,exclusiveGroup:`route-transition`}),transition:r,contract:Object.freeze({domContentOwner:!0,role:`non-opaque-modal-edge-underlay`}),uniforms:Object.freeze({uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,description:`Expansion progress.`}),uOrigin:Object.freeze({type:`vec2`,default:[.5,.5],description:`Normalized trigger origin.`}),uFromRect:Object.freeze({type:`vec4`,default:[.02,.02,.02,.02],description:`Collapsed rect.`}),uToRect:Object.freeze({type:`vec4`,default:[.2,.16,.6,.68],description:`Expanded modal rect.`}),uTint:Object.freeze({type:`vec3`,default:[.48,.82,1],description:`Modal edge tint.`}),uIntensity:Object.freeze({type:`float`,default:.3,min:0,max:.62,description:`Edge opacity.`}),uSpeed:Object.freeze({type:`float`,default:.45,min:0,max:2,description:`Edge shimmer speed.`}),uDensity:Object.freeze({type:`float`,default:.5,min:.2,max:1,description:`Edge detail density.`}),uScale:Object.freeze({type:`float`,default:1,min:.7,max:1.4,description:`Edge scale.`}),uCornerRadius:Object.freeze({type:`float`,default:.12,min:0,max:.45,description:`Corner radius.`})}),quality:Object.freeze({high:Object.freeze({edgeFactor:1,alphaCap:1}),medium:Object.freeze({edgeFactor:.72,alphaCap:.78}),low:Object.freeze({edgeFactor:.46,alphaCap:.52})}),cost:Object.freeze({budgetUnits:Object.freeze({high:1.2,medium:.78,low:.38}),gpuMilliseconds:Object.freeze({high:.26,medium:.17,low:.09}),fill:`partial`,allocation:`event-only-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`main-only`,render:`main-or-offscreen`})});function a(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function o(e,t,n,r=0,i=1){let a=Array.isArray(e)||ArrayBuffer.isView(e)?e:t;return Array.from({length:n},(e,n)=>{let o=Number(a[n]);return Number.isFinite(o)?Math.min(i,Math.max(r,o)):Number(t[n])})}function s(e,t){return o(e,t,4,-1,2).map((e,t)=>t<2?e:Math.max(.001,e))}function c(e,t,n){let r=1-.001**(Math.max(0,n)/.3);return e+(t-e)*r}function l(){return typeof globalThis.matchMedia==`function`&&globalThis.matchMedia(`(prefers-reduced-motion: reduce)`).matches}function u(r,u={}){let d=r.gl,f=r.createProgram({vertexSource:t,fragmentSource:n,label:i.id}),p=Object.freeze({progress:d.getUniformLocation(f.program,`uProgress`),origin:d.getUniformLocation(f.program,`uOrigin`),fromRect:d.getUniformLocation(f.program,`uFromRect`),toRect:d.getUniformLocation(f.program,`uToRect`),tint:d.getUniformLocation(f.program,`uTint`),intensity:d.getUniformLocation(f.program,`uIntensity`),speed:d.getUniformLocation(f.program,`uSpeed`),density:d.getUniformLocation(f.program,`uDensity`),scale:d.getUniformLocation(f.program,`uScale`),cornerRadius:d.getUniformLocation(f.program,`uCornerRadius`)}),m=i.uniforms,h=a(u.uProgress,m.uProgress),g=h,_=o(u.uOrigin,m.uOrigin.default,2),v=_.slice(),y=s(u.uFromRect,m.uFromRect.default),b=y.slice(),x=s(u.uToRect,m.uToRect.default),S=x.slice(),C=o(u.uTint,m.uTint.default,3).map(t=>e(t)),w=C.slice(),T=a(u.uIntensity,m.uIntensity),E=T,D=a(u.uSpeed,m.uSpeed),O=D,k=a(u.uDensity,m.uDensity),A=k,j=a(u.uScale,m.uScale),M=j,N=a(u.uCornerRadius,m.uCornerRadius),P=N,F=i.quality.medium,I=l(),L=!I,R=!1;function z(t={}){Object.prototype.hasOwnProperty.call(t,`uProgress`)&&(h=a(t.uProgress,m.uProgress)),Object.prototype.hasOwnProperty.call(t,`uOrigin`)&&(_=o(t.uOrigin,_,2)),Object.prototype.hasOwnProperty.call(t,`uFromRect`)&&(y=s(t.uFromRect,y)),Object.prototype.hasOwnProperty.call(t,`uToRect`)&&(x=s(t.uToRect,x)),Object.prototype.hasOwnProperty.call(t,`uTint`)&&(C=o(t.uTint,C,3).map(t=>e(t))),Object.prototype.hasOwnProperty.call(t,`uIntensity`)&&(T=a(t.uIntensity,m.uIntensity)),Object.prototype.hasOwnProperty.call(t,`uSpeed`)&&(D=a(t.uSpeed,m.uSpeed)),Object.prototype.hasOwnProperty.call(t,`uDensity`)&&(k=a(t.uDensity,m.uDensity)),Object.prototype.hasOwnProperty.call(t,`uScale`)&&(j=a(t.uScale,m.uScale)),Object.prototype.hasOwnProperty.call(t,`uCornerRadius`)&&(N=a(t.uCornerRadius,m.uCornerRadius))}function B(e,t){F=i.quality[e]||i.quality.medium,L=!!t&&!I}function V(){}function H(e){if(I){g=1;return}g=c(g,h,e);for(let t=0;t<2;t+=1)v[t]=c(v[t],_[t],e);for(let t=0;t<4;t+=1)b[t]=c(b[t],y[t],e),S[t]=c(S[t],x[t],e);for(let t=0;t<3;t+=1)w[t]=c(w[t],C[t],e);E=c(E,T,e),O=c(O,D,e),A=c(A,k,e),M=c(M,j,e),P=c(P,N,e)}function U(t){R||!L||g<=.001||g>=.999||(d.useProgram(f.program),r.bindEngineGlobals(f,t),p.progress!==null&&d.uniform1f(p.progress,e(g)),p.origin!==null&&d.uniform2fv(p.origin,v),p.fromRect!==null&&d.uniform4fv(p.fromRect,b),p.toRect!==null&&d.uniform4fv(p.toRect,S),p.tint!==null&&d.uniform3fv(p.tint,w),p.intensity!==null&&d.uniform1f(p.intensity,E*F.alphaCap),p.speed!==null&&d.uniform1f(p.speed,O),p.density!==null&&d.uniform1f(p.density,A*F.edgeFactor),p.scale!==null&&d.uniform1f(p.scale,M),p.cornerRadius!==null&&d.uniform1f(p.cornerRadius,P),d.bindVertexArray(r.fullscreenVao),d.drawArrays(d.TRIANGLES,0,3))}function W(){R||(R=!0,r.disposeProgram(f))}return{setParameters:z,setQuality:B,resize:V,simulate:H,render:U,dispose:W}}function d(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-modal-expand-fallback`,r=e.getAttribute(n);e.setAttribute(n,String(t.reason||`dom-modal-transition`));let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{d as applyDomFallback,u as createEffect,i as manifest,r as transition};