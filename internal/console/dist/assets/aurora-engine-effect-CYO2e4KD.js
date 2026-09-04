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
uniform vec2 uResolution;
uniform float uPixelRatio;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;
uniform float uTime;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uFiberScale;
uniform float uFiberStrength;
uniform float uAge;
uniform float uCornerRadius;
uniform float uDetail;
uniform float uAlphaCap;

out vec4 fragColor;

float hash12(vec2 point) {
  return fract(sin(dot(point, vec2(23.17, 91.43))) * 317.29);
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
  vec2 physical = max(uElementRect.zw * uResolution, vec2(1.0));
  vec2 pixel = vUv * physical;
  float dpr = max(uPixelRatio, 1.0);
  float fiberCell = max(1.0, uFiberScale * dpr);
  float broad = hash12(floor(pixel / fiberCell));
  float fine = hash12(floor(pixel / dpr));
  float threads = 0.5 + 0.5 * sin(pixel.y / fiberCell * 2.8 + pixel.x / fiberCell * 0.32 + uTime * 0.08);
  float texture = mix(broad, fine, uDetail) * 0.72 + threads * 0.28;
  vec3 paper = mix(uAtmoNear, uAtmoFar, 0.28 + vUv.y * 0.44);
  paper = mix(paper, uTint, 0.45 + uAge * 0.18);
  paper += (texture - 0.5) * uFiberStrength;
  float alpha = shape * uIntensity * uAlphaCap * (0.7 + texture * 0.3);
  fragColor = vec4(max(paper, vec3(0.0)), alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`paper-texture`,title:`Paper Texture`,composition:Object.freeze({slot:`foreground`,blend:`alpha`,zIndex:39,priority:39,exclusiveGroup:`material-surface`}),uniforms:Object.freeze({uElementRect:Object.freeze({type:`vec4`,default:[.25,.25,.5,.5],description:`Normalized [left, bottom, width, height] target rect.`}),uTint:Object.freeze({type:`vec3`,default:[.88,.86,.8],description:`Paper base tint.`}),uIntensity:Object.freeze({type:`float`,default:.11,min:0,max:.35,step:.01,description:`Subtle surface opacity.`}),uFiberScale:Object.freeze({type:`float`,default:7,min:1,max:32,step:.25,description:`Fiber cell size in CSS pixels before DPR scaling.`}),uFiberStrength:Object.freeze({type:`float`,default:.12,min:0,max:.35,step:.01,description:`Fiber contrast.`}),uAge:Object.freeze({type:`float`,default:.25,min:0,max:1,step:.01,description:`Warm aged-paper bias.`}),uCornerRadius:Object.freeze({type:`float`,default:.06,min:0,max:.3,step:.01,description:`Rounded paper mask.`})}),quality:Object.freeze({high:Object.freeze({detail:.8,alphaCap:1}),medium:Object.freeze({detail:.52,alphaCap:.72}),low:Object.freeze({detail:.2,alphaCap:.46})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.28,medium:.18,low:.07}),gpuMilliseconds:Object.freeze({high:.08,medium:.052,low:.028}),fill:`partial`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r,i){let a=Number(e&&e[t]);return Number.isFinite(a)?Math.min(i,Math.max(r,a)):n[t]}function a(e,t){return[i(e,0,t,-.5,1.5),i(e,1,t,-.5,1.5),i(e,2,t,.001,2),i(e,3,t,.001,2)]}function o(e,t){return[i(e,0,t,0,1),i(e,1,t,0,1),i(e,2,t,0,1)]}function s(e,t,n){return e+(t-e)*(1-1e-4**Math.max(0,n))}function c(i,c={}){let l=i.gl,u=i.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),d=Object.freeze({rect:l.getUniformLocation(u.program,`uElementRect`),tint:l.getUniformLocation(u.program,`uTint`),intensity:l.getUniformLocation(u.program,`uIntensity`),fiberScale:l.getUniformLocation(u.program,`uFiberScale`),fiberStrength:l.getUniformLocation(u.program,`uFiberStrength`),age:l.getUniformLocation(u.program,`uAge`),cornerRadius:l.getUniformLocation(u.program,`uCornerRadius`),detail:l.getUniformLocation(u.program,`uDetail`),alphaCap:l.getUniformLocation(u.program,`uAlphaCap`)}),f=n.uniforms,p=a(c.uElementRect,f.uElementRect.default),m=p.slice(),h=o(c.uTint,f.uTint.default),g=h.slice(),_=r(c.uIntensity,f.uIntensity),v=_,y=r(c.uFiberScale,f.uFiberScale),b=y,x=r(c.uFiberStrength,f.uFiberStrength),S=x,C=r(c.uAge,f.uAge),w=C,T=r(c.uCornerRadius,f.uCornerRadius),E=T,D=n.quality.medium,O=!0,k=!1;function A(e={}){Object.prototype.hasOwnProperty.call(e,`uElementRect`)&&(p=a(e.uElementRect,p)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(h=o(e.uTint,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,f.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uFiberScale`)&&(y=r(e.uFiberScale,f.uFiberScale)),Object.prototype.hasOwnProperty.call(e,`uFiberStrength`)&&(x=r(e.uFiberStrength,f.uFiberStrength)),Object.prototype.hasOwnProperty.call(e,`uAge`)&&(C=r(e.uAge,f.uAge)),Object.prototype.hasOwnProperty.call(e,`uCornerRadius`)&&(T=r(e.uCornerRadius,f.uCornerRadius))}function j(e,t){D=n.quality[e]||n.quality.medium,O=!!t}function M(){}function N(e){for(let t=0;t<4;t+=1)m[t]=s(m[t],p[t],e);for(let t=0;t<3;t+=1)g[t]=s(g[t],h[t],e);v=s(v,_,e),b=s(b,y,e),S=s(S,x,e),w=s(w,C,e),E=s(E,T,e)}function P(e){k||!O||(l.useProgram(u.program),i.bindEngineGlobals(u,e),d.rect!==null&&l.uniform4f(d.rect,m[0],m[1],m[2],m[3]),d.tint!==null&&l.uniform3f(d.tint,g[0],g[1],g[2]),d.intensity!==null&&l.uniform1f(d.intensity,v),d.fiberScale!==null&&l.uniform1f(d.fiberScale,b),d.fiberStrength!==null&&l.uniform1f(d.fiberStrength,S),d.age!==null&&l.uniform1f(d.age,w),d.cornerRadius!==null&&l.uniform1f(d.cornerRadius,E),d.detail!==null&&l.uniform1f(d.detail,D.detail),d.alphaCap!==null&&l.uniform1f(d.alphaCap,D.alphaCap),l.bindVertexArray(i.fullscreenVao),l.drawArrays(l.TRIANGLES,0,3))}function F(){k||(k=!0,i.disposeProgram(u))}return{setParameters:A,setQuality:j,resize:M,simulate:N,render:P,dispose:F}}function l(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-paper-texture-fallback`,r=e.getAttribute(n);e.setAttribute(n,t.state||`active`);let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{l as applyDomFallback,c as createEffect,n as manifest};