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
uniform vec3 uAtmoVoid;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uDensity;
uniform float uFalloff;
uniform float uSpeed;
uniform float uDepth;
uniform float uCornerRadius;
uniform float uDetail;
uniform float uAlphaCap;

out vec4 fragColor;

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
  float verticalDepth = clamp((1.0 - vUv.y) * (0.72 + uDepth * 0.7), 0.0, 1.0);
  float sideDepth = 1.0 - smoothstep(0.18, 0.74, length(point));
  float haze = pow(verticalDepth, max(uFalloff, 0.1)) * (0.46 + sideDepth * 0.54);
  float wisps = 0.5 + 0.5 * sin((vUv.x * 8.0 + vUv.y * 5.0) * uDensity + uTime * uSpeed);
  float fine = 0.5 + 0.5 * sin(vUv.x * 31.0 - vUv.y * 17.0 + uTime * uSpeed * 0.6);
  float veil = mix(wisps, fine, uDetail);
  vec3 color = mix(uAtmoVoid, uAtmoNear, 0.32 + verticalDepth * 0.48);
  color = mix(color, uTint, 0.2 + veil * 0.24);
  float alpha = shape * haze * uIntensity * uAlphaCap * (0.52 + veil * 0.48);
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`depth-fog`,title:`Depth Fog`,composition:Object.freeze({slot:`foreground`,blend:`alpha`,zIndex:38,priority:38,exclusiveGroup:`material-surface`}),uniforms:Object.freeze({uElementRect:Object.freeze({type:`vec4`,default:[.25,.25,.5,.5],description:`Normalized [left, bottom, width, height] target rect.`}),uTint:Object.freeze({type:`vec3`,default:[.3,.56,.7],description:`Fog tint.`}),uIntensity:Object.freeze({type:`float`,default:.16,min:0,max:.5,step:.01,description:`Haze opacity.`}),uDensity:Object.freeze({type:`float`,default:.9,min:.2,max:3,step:.01,description:`Wisp density.`}),uFalloff:Object.freeze({type:`float`,default:1.7,min:.3,max:4,step:.01,description:`Depth falloff exponent.`}),uSpeed:Object.freeze({type:`float`,default:.16,min:-1,max:1,step:.01,description:`Fixed-clock haze drift.`}),uDepth:Object.freeze({type:`float`,default:.55,min:0,max:1,step:.01,description:`Perceived panel depth.`}),uCornerRadius:Object.freeze({type:`float`,default:.08,min:0,max:.3,step:.01,description:`Rounded panel mask.`})}),quality:Object.freeze({high:Object.freeze({detail:.8,alphaCap:1}),medium:Object.freeze({detail:.5,alphaCap:.72}),low:Object.freeze({detail:.18,alphaCap:.46})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.32,medium:.2,low:.08}),gpuMilliseconds:Object.freeze({high:.09,medium:.058,low:.03}),fill:`partial`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r,i){let a=Number(e&&e[t]);return Number.isFinite(a)?Math.min(i,Math.max(r,a)):n[t]}function a(e,t){return[i(e,0,t,-.5,1.5),i(e,1,t,-.5,1.5),i(e,2,t,.001,2),i(e,3,t,.001,2)]}function o(e,t){return[i(e,0,t,0,1),i(e,1,t,0,1),i(e,2,t,0,1)]}function s(e,t,n){return e+(t-e)*(1-1e-4**Math.max(0,n))}function c(i,c={}){let l=i.gl,u=i.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),d=Object.freeze({rect:l.getUniformLocation(u.program,`uElementRect`),tint:l.getUniformLocation(u.program,`uTint`),intensity:l.getUniformLocation(u.program,`uIntensity`),density:l.getUniformLocation(u.program,`uDensity`),falloff:l.getUniformLocation(u.program,`uFalloff`),speed:l.getUniformLocation(u.program,`uSpeed`),depth:l.getUniformLocation(u.program,`uDepth`),cornerRadius:l.getUniformLocation(u.program,`uCornerRadius`),detail:l.getUniformLocation(u.program,`uDetail`),alphaCap:l.getUniformLocation(u.program,`uAlphaCap`)}),f=n.uniforms,p=a(c.uElementRect,f.uElementRect.default),m=p.slice(),h=o(c.uTint,f.uTint.default),g=h.slice(),_=r(c.uIntensity,f.uIntensity),v=_,y=r(c.uDensity,f.uDensity),b=y,x=r(c.uFalloff,f.uFalloff),S=x,C=r(c.uSpeed,f.uSpeed),w=C,T=r(c.uDepth,f.uDepth),E=T,D=r(c.uCornerRadius,f.uCornerRadius),O=D,k=n.quality.medium,A=!0,j=!1;function M(e={}){Object.prototype.hasOwnProperty.call(e,`uElementRect`)&&(p=a(e.uElementRect,p)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(h=o(e.uTint,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,f.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uDensity`)&&(y=r(e.uDensity,f.uDensity)),Object.prototype.hasOwnProperty.call(e,`uFalloff`)&&(x=r(e.uFalloff,f.uFalloff)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(C=r(e.uSpeed,f.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uDepth`)&&(T=r(e.uDepth,f.uDepth)),Object.prototype.hasOwnProperty.call(e,`uCornerRadius`)&&(D=r(e.uCornerRadius,f.uCornerRadius))}function N(e,t){k=n.quality[e]||n.quality.medium,A=!!t}function P(){}function F(e){for(let t=0;t<4;t+=1)m[t]=s(m[t],p[t],e);for(let t=0;t<3;t+=1)g[t]=s(g[t],h[t],e);v=s(v,_,e),b=s(b,y,e),S=s(S,x,e),w=s(w,C,e),E=s(E,T,e),O=s(O,D,e)}function I(e){j||!A||(l.useProgram(u.program),i.bindEngineGlobals(u,e),d.rect!==null&&l.uniform4f(d.rect,m[0],m[1],m[2],m[3]),d.tint!==null&&l.uniform3f(d.tint,g[0],g[1],g[2]),d.intensity!==null&&l.uniform1f(d.intensity,v),d.density!==null&&l.uniform1f(d.density,b),d.falloff!==null&&l.uniform1f(d.falloff,S),d.speed!==null&&l.uniform1f(d.speed,w),d.depth!==null&&l.uniform1f(d.depth,E),d.cornerRadius!==null&&l.uniform1f(d.cornerRadius,O),d.detail!==null&&l.uniform1f(d.detail,k.detail),d.alphaCap!==null&&l.uniform1f(d.alphaCap,k.alphaCap),l.bindVertexArray(i.fullscreenVao),l.drawArrays(l.TRIANGLES,0,3))}function L(){j||(j=!0,i.disposeProgram(u))}return{setParameters:M,setQuality:N,resize:P,simulate:F,render:I,dispose:L}}function l(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-depth-fog-fallback`,r=e.getAttribute(n);e.setAttribute(n,t.state||`active`);let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{l as applyDomFallback,c as createEffect,n as manifest};