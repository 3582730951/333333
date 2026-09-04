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
uniform vec4 uFromRect;
uniform vec4 uToRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
uniform float uCornerRadius;
uniform float uGeometryReady;
out vec4 fragColor;

float roundedBox(vec2 point, vec2 halfSize, float radius) {
  vec2 q = abs(point) - halfSize + radius;
  return length(max(q, vec2(0.0))) + min(max(q.x, q.y), 0.0) - radius;
}

void main() {
  vec4 rect = mix(uFromRect, uToRect, uProgress);
  vec2 centre = rect.xy + rect.zw * 0.5;
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  vec2 point = (vUv - centre) * aspect;
  vec2 halfSize = max(rect.zw * 0.5 * aspect, vec2(0.001));
  float radius = min(min(halfSize.x, halfSize.y) * uCornerRadius * 2.0, 0.45);
  float distanceToBox = roundedBox(point, halfSize, radius);
  float lineWidth = mix(0.003, 0.012, uDensity) * uScale;
  float outline = 1.0 - smoothstep(lineWidth, lineWidth * 2.4, abs(distanceToBox));
  float fill = 1.0 - smoothstep(0.0, lineWidth * 2.0, distanceToBox);
  float shimmer = 0.82 + 0.18 * sin((point.x + point.y) * mix(20.0, 64.0, uDensity) - uTime * uSpeed * 3.0);
  vec3 colour = mix(uTint, uAtmoGlow, 0.22);
  float alpha = (outline * 0.8 + fill * 0.08) * shimmer * uIntensity * uGeometryReady * (0.58 + 0.42 * uQuality);
  fragColor = vec4(colour, alpha);
}
`,r=Object.freeze({durationMs:300,durationToken:`--pool-motion-slow`,easing:`cubic-bezier(.2, 0, 0, 1)`,easingToken:`--pool-ease-emphasized`,reducedMotion:`skip-to-end`}),i=Object.freeze({schemaVersion:1,id:`shared-element-flight`,title:`Shared element flight`,composition:Object.freeze({slot:`foreground`,blend:`alpha`,zIndex:45,priority:45,exclusiveGroup:`route-transition`}),transition:r,contract:Object.freeze({requiresCrossRouteGeometry:!0,geometryOwner:`route-coordinator`,domContentOwner:!0}),uniforms:Object.freeze({uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,description:`Flight progress.`}),uFromRect:Object.freeze({type:`vec4`,default:[.35,.4,.3,.16],description:`Source [x,y,width,height] in normalized viewport coordinates.`}),uToRect:Object.freeze({type:`vec4`,default:[.18,.2,.64,.58],description:`Destination [x,y,width,height] in normalized viewport coordinates.`}),uTint:Object.freeze({type:`vec3`,default:[.46,.82,1],description:`Flight outline tint.`}),uIntensity:Object.freeze({type:`float`,default:.32,min:0,max:.66,description:`Outline opacity.`}),uSpeed:Object.freeze({type:`float`,default:.7,min:0,max:2,description:`Trail shimmer speed.`}),uDensity:Object.freeze({type:`float`,default:.52,min:.2,max:1,description:`Trail detail density.`}),uScale:Object.freeze({type:`float`,default:1,min:.7,max:1.4,description:`Flight outline scale.`}),uCornerRadius:Object.freeze({type:`float`,default:.1,min:0,max:.45,description:`Normalized corner radius.`}),uGeometryReady:Object.freeze({type:`float`,default:1,min:0,max:1,description:`1 when route geometry was captured.`})}),quality:Object.freeze({high:Object.freeze({trailFactor:1,alphaCap:1}),medium:Object.freeze({trailFactor:.72,alphaCap:.78}),low:Object.freeze({trailFactor:.46,alphaCap:.52})}),cost:Object.freeze({budgetUnits:Object.freeze({high:1.35,medium:.88,low:.42}),gpuMilliseconds:Object.freeze({high:.3,medium:.2,low:.11}),fill:`partial`,allocation:`event-only-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`main-only`,render:`main-or-offscreen`})});function a(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function o(e,t){let n=Array.isArray(e)||ArrayBuffer.isView(e)?e:t;return[0,1,2,3].map(e=>{let r=Number(n[e]),i=e<2?-1:.001;return Number.isFinite(r)?Math.min(2,Math.max(i,r)):Number(t[e])})}function s(t,n){let r=Array.isArray(t)||ArrayBuffer.isView(t)?t:n;return[0,1,2].map(t=>e(r[t],n[t]))}function c(e,t,n){let r=1-.001**(Math.max(0,n)/.3);return e+(t-e)*r}function l(){return typeof globalThis.matchMedia==`function`&&globalThis.matchMedia(`(prefers-reduced-motion: reduce)`).matches}function u(r,u={}){let d=r.gl,f=r.createProgram({vertexSource:t,fragmentSource:n,label:i.id}),p=Object.freeze({progress:d.getUniformLocation(f.program,`uProgress`),fromRect:d.getUniformLocation(f.program,`uFromRect`),toRect:d.getUniformLocation(f.program,`uToRect`),tint:d.getUniformLocation(f.program,`uTint`),intensity:d.getUniformLocation(f.program,`uIntensity`),speed:d.getUniformLocation(f.program,`uSpeed`),density:d.getUniformLocation(f.program,`uDensity`),scale:d.getUniformLocation(f.program,`uScale`),cornerRadius:d.getUniformLocation(f.program,`uCornerRadius`),geometryReady:d.getUniformLocation(f.program,`uGeometryReady`)}),m=i.uniforms,h=a(u.uProgress,m.uProgress),g=h,_=o(u.uFromRect,m.uFromRect.default),v=_.slice(),y=o(u.uToRect,m.uToRect.default),b=y.slice(),x=s(u.uTint,m.uTint.default),S=x.slice(),C=a(u.uIntensity,m.uIntensity),w=C,T=a(u.uSpeed,m.uSpeed),E=T,D=a(u.uDensity,m.uDensity),O=D,k=a(u.uScale,m.uScale),A=k,j=a(u.uCornerRadius,m.uCornerRadius),M=j,N=a(u.uGeometryReady,m.uGeometryReady),P=N,F=i.quality.medium,I=l(),L=!I,R=!1;function z(e={}){Object.prototype.hasOwnProperty.call(e,`uProgress`)&&(h=a(e.uProgress,m.uProgress)),Object.prototype.hasOwnProperty.call(e,`uFromRect`)&&(_=o(e.uFromRect,_)),Object.prototype.hasOwnProperty.call(e,`uToRect`)&&(y=o(e.uToRect,y)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(x=s(e.uTint,x)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(C=a(e.uIntensity,m.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(T=a(e.uSpeed,m.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uDensity`)&&(D=a(e.uDensity,m.uDensity)),Object.prototype.hasOwnProperty.call(e,`uScale`)&&(k=a(e.uScale,m.uScale)),Object.prototype.hasOwnProperty.call(e,`uCornerRadius`)&&(j=a(e.uCornerRadius,m.uCornerRadius)),Object.prototype.hasOwnProperty.call(e,`uGeometryReady`)&&(N=a(e.uGeometryReady,m.uGeometryReady))}function B(e,t){F=i.quality[e]||i.quality.medium,L=!!t&&!I}function V(){}function H(e){if(I){g=1;return}g=c(g,h,e);for(let t=0;t<4;t+=1)v[t]=c(v[t],_[t],e),b[t]=c(b[t],y[t],e);for(let t=0;t<3;t+=1)S[t]=c(S[t],x[t],e);w=c(w,C,e),E=c(E,T,e),O=c(O,D,e),A=c(A,k,e),M=c(M,j,e),P=c(P,N,e)}function U(t){R||!L||g<=.001||g>=.999||P<=.001||(d.useProgram(f.program),r.bindEngineGlobals(f,t),p.progress!==null&&d.uniform1f(p.progress,e(g)),p.fromRect!==null&&d.uniform4fv(p.fromRect,v),p.toRect!==null&&d.uniform4fv(p.toRect,b),p.tint!==null&&d.uniform3fv(p.tint,S),p.intensity!==null&&d.uniform1f(p.intensity,w*F.alphaCap),p.speed!==null&&d.uniform1f(p.speed,E),p.density!==null&&d.uniform1f(p.density,O*F.trailFactor),p.scale!==null&&d.uniform1f(p.scale,A),p.cornerRadius!==null&&d.uniform1f(p.cornerRadius,M),p.geometryReady!==null&&d.uniform1f(p.geometryReady,P),d.bindVertexArray(r.fullscreenVao),d.drawArrays(d.TRIANGLES,0,3))}function W(){R||(R=!0,r.disposeProgram(f))}return{setParameters:z,setQuality:B,resize:V,simulate:H,render:U,dispose:W}}function d(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-shared-element-flight-fallback`,r=e.getAttribute(n);e.setAttribute(n,String(t.reason||`cross-route-geometry-unavailable`));let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{d as applyDomFallback,u as createEffect,i as manifest,r as transition};