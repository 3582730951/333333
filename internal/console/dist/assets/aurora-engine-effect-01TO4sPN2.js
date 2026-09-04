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
uniform vec3 uAtmoGlow;
uniform vec3 uAtmoFar;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uBorderWidth;
uniform float uSoftness;
uniform float uCornerRadius;
uniform float uSegmentDensity;
uniform float uShineStrength;
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
  float distanceToEdge = abs(roundedBox(point, vec2(0.5 * aspect, 0.5), radius));
  float core = 1.0 - smoothstep(0.0, uBorderWidth, distanceToEdge);
  float halo = 1.0 - smoothstep(uBorderWidth, uBorderWidth + uSoftness, distanceToEdge);
  float sweep = 0.5 + 0.5 * sin((vUv.x - vUv.y) * 18.0 * uSegmentDensity + uTime * uSpeed);
  vec3 base = mix(uAtmoFar, uAtmoGlow, sweep * uShineStrength);
  vec3 color = mix(base, uTint, 0.55 + sweep * 0.25);
  float alpha = max(core, halo * 0.62) * uIntensity * uAlphaCap;
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`glow-border`,title:`Glow Border`,composition:Object.freeze({slot:`foreground`,blend:`additive`,zIndex:44,priority:44,exclusiveGroup:`material-outline`}),uniforms:Object.freeze({uElementRect:Object.freeze({type:`vec4`,default:[.25,.25,.5,.5],description:`Normalized [left, bottom, width, height] target rect.`}),uTint:Object.freeze({type:`vec3`,default:[.4,.9,1],description:`Border color blended with the Aurora glow palette.`}),uIntensity:Object.freeze({type:`float`,default:.52,min:0,max:1}),uSpeed:Object.freeze({type:`float`,default:.6,min:0,max:3}),uBorderWidth:Object.freeze({type:`float`,default:.018,min:.002,max:.1}),uSoftness:Object.freeze({type:`float`,default:.04,min:.002,max:.16}),uCornerRadius:Object.freeze({type:`float`,default:.11,min:0,max:.3})}),quality:Object.freeze({high:Object.freeze({segmentDensity:1,shineStrength:1,alphaCap:1}),medium:Object.freeze({segmentDensity:.72,shineStrength:.7,alphaCap:.78}),low:Object.freeze({segmentDensity:.42,shineStrength:0,alphaCap:.48})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.45,medium:.28,low:.12}),gpuMilliseconds:Object.freeze({high:.11,medium:.07,low:.04}),fill:`partial`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r,i){let a=Number(e&&e[t]);return Number.isFinite(a)?Math.min(i,Math.max(r,a)):n[t]}function a(e,t){return[i(e,0,t,-.5,1.5),i(e,1,t,-.5,1.5),i(e,2,t,.001,2),i(e,3,t,.001,2)]}function o(e,t){return[i(e,0,t,0,1),i(e,1,t,0,1),i(e,2,t,0,1)]}function s(e,t,n){return e+(t-e)*(1-1e-4**Math.max(0,n))}function c(i,c={}){let l=i.gl,u=i.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),d=Object.freeze({rect:l.getUniformLocation(u.program,`uElementRect`),tint:l.getUniformLocation(u.program,`uTint`),intensity:l.getUniformLocation(u.program,`uIntensity`),speed:l.getUniformLocation(u.program,`uSpeed`),borderWidth:l.getUniformLocation(u.program,`uBorderWidth`),softness:l.getUniformLocation(u.program,`uSoftness`),cornerRadius:l.getUniformLocation(u.program,`uCornerRadius`),segmentDensity:l.getUniformLocation(u.program,`uSegmentDensity`),shineStrength:l.getUniformLocation(u.program,`uShineStrength`),alphaCap:l.getUniformLocation(u.program,`uAlphaCap`)}),f=n.uniforms,p=a(c.uElementRect,f.uElementRect.default),m=p.slice(),h=o(c.uTint,f.uTint.default),g=h.slice(),_=r(c.uIntensity,f.uIntensity),v=_,y=r(c.uSpeed,f.uSpeed),b=y,x=r(c.uBorderWidth,f.uBorderWidth),S=x,C=r(c.uSoftness,f.uSoftness),w=C,T=r(c.uCornerRadius,f.uCornerRadius),E=T,D=n.quality.medium,O=!0,k=!1;function A(e={}){Object.prototype.hasOwnProperty.call(e,`uElementRect`)&&(p=a(e.uElementRect,p)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(h=o(e.uTint,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,f.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(y=r(e.uSpeed,f.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uBorderWidth`)&&(x=r(e.uBorderWidth,f.uBorderWidth)),Object.prototype.hasOwnProperty.call(e,`uSoftness`)&&(C=r(e.uSoftness,f.uSoftness)),Object.prototype.hasOwnProperty.call(e,`uCornerRadius`)&&(T=r(e.uCornerRadius,f.uCornerRadius))}function j(e,t){D=n.quality[e]||n.quality.medium,O=!!t}function M(){}function N(e){for(let t=0;t<4;t+=1)m[t]=s(m[t],p[t],e);for(let t=0;t<3;t+=1)g[t]=s(g[t],h[t],e);v=s(v,_,e),b=s(b,y,e),S=s(S,x,e),w=s(w,C,e),E=s(E,T,e)}function P(e){k||!O||(l.useProgram(u.program),i.bindEngineGlobals(u,e),d.rect!==null&&l.uniform4f(d.rect,m[0],m[1],m[2],m[3]),d.tint!==null&&l.uniform3f(d.tint,g[0],g[1],g[2]),d.intensity!==null&&l.uniform1f(d.intensity,v),d.speed!==null&&l.uniform1f(d.speed,b),d.borderWidth!==null&&l.uniform1f(d.borderWidth,S),d.softness!==null&&l.uniform1f(d.softness,w),d.cornerRadius!==null&&l.uniform1f(d.cornerRadius,E),d.segmentDensity!==null&&l.uniform1f(d.segmentDensity,D.segmentDensity),d.shineStrength!==null&&l.uniform1f(d.shineStrength,D.shineStrength),d.alphaCap!==null&&l.uniform1f(d.alphaCap,D.alphaCap),l.bindVertexArray(i.fullscreenVao),l.drawArrays(l.TRIANGLES,0,3))}function F(){k||(k=!0,i.disposeProgram(u))}return{setParameters:A,setQuality:j,resize:M,simulate:N,render:P,dispose:F}}function l(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-glow-border-fallback`,r=e.getAttribute(n);e.setAttribute(n,t.state||`active`);let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{l as applyDomFallback,c as createEffect,n as manifest};