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
uniform vec3 uAtmoGlow;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uRefraction;
uniform float uBlur;
uniform float uCornerRadius;
uniform float uWaveDensity;
uniform float uOpacityCap;

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
  float distanceToCorner = roundedBox(point, vec2(0.5 * aspect, 0.5), radius);
  float shape = 1.0 - smoothstep(0.0, 0.012, distanceToCorner);

  float phase = uTime * uSpeed;
  vec2 normal = vec2(
    sin((point.y * 19.0 + phase) * uWaveDensity),
    cos((point.x * 17.0 - phase * 0.8) * uWaveDensity)
  );
  vec2 refracted = clamp(vUv + normal * uRefraction, 0.0, 1.0);
  vec3 horizon = mix(uAtmoVoid, uAtmoNear, refracted.y);
  vec3 glow = mix(uAtmoFar, uAtmoGlow, refracted.x);
  float cloud = 0.5 + 0.5 * sin((refracted.x + refracted.y) * 8.0 + phase * 0.35);
  vec3 proxyBackdrop = mix(horizon, glow, mix(0.28, 0.72, cloud * uBlur));
  float fresnel = pow(1.0 - clamp(length(point) * 1.3, 0.0, 1.0), 2.4);
  vec3 color = mix(proxyBackdrop, uTint, 0.18 + fresnel * 0.32);
  float alpha = shape * uIntensity * uOpacityCap * (0.62 + fresnel * 0.38);
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`glass-refraction`,title:`Glass Refraction`,composition:Object.freeze({slot:`foreground`,blend:`alpha`,zIndex:42,priority:42,exclusiveGroup:`material-surface`}),uniforms:Object.freeze({uElementRect:Object.freeze({type:`vec4`,default:[.25,.25,.5,.5],description:`Normalized [left, bottom, width, height] target rect.`}),uTint:Object.freeze({type:`vec3`,default:[.66,.85,1],description:`Glass tint mixed with the Aurora palette.`}),uIntensity:Object.freeze({type:`float`,default:.22,min:0,max:.5}),uSpeed:Object.freeze({type:`float`,default:.34,min:0,max:2}),uRefraction:Object.freeze({type:`float`,default:.018,min:0,max:.06}),uBlur:Object.freeze({type:`float`,default:.58,min:0,max:1}),uCornerRadius:Object.freeze({type:`float`,default:.11,min:0,max:.3})}),quality:Object.freeze({high:Object.freeze({waveDensity:1,opacityCap:1}),medium:Object.freeze({waveDensity:.72,opacityCap:.82}),low:Object.freeze({waveDensity:.42,opacityCap:.58})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.7,medium:.45,low:.22}),gpuMilliseconds:Object.freeze({high:.18,medium:.12,low:.07}),fill:`partial`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r,i){let a=Number(e&&e[t]);return Number.isFinite(a)?Math.min(i,Math.max(r,a)):n[t]}function a(e,t){return[i(e,0,t,-.5,1.5),i(e,1,t,-.5,1.5),i(e,2,t,.001,2),i(e,3,t,.001,2)]}function o(e,t){return[i(e,0,t,0,1),i(e,1,t,0,1),i(e,2,t,0,1)]}function s(e,t,n){return e+(t-e)*(1-1e-4**Math.max(0,n))}function c(i,c={}){let l=i.gl,u=i.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),d=Object.freeze({rect:l.getUniformLocation(u.program,`uElementRect`),tint:l.getUniformLocation(u.program,`uTint`),intensity:l.getUniformLocation(u.program,`uIntensity`),speed:l.getUniformLocation(u.program,`uSpeed`),refraction:l.getUniformLocation(u.program,`uRefraction`),blur:l.getUniformLocation(u.program,`uBlur`),cornerRadius:l.getUniformLocation(u.program,`uCornerRadius`),waveDensity:l.getUniformLocation(u.program,`uWaveDensity`),opacityCap:l.getUniformLocation(u.program,`uOpacityCap`)}),f=n.uniforms,p=a(c.uElementRect,f.uElementRect.default),m=p.slice(),h=o(c.uTint,f.uTint.default),g=h.slice(),_=r(c.uIntensity,f.uIntensity),v=_,y=r(c.uSpeed,f.uSpeed),b=y,x=r(c.uRefraction,f.uRefraction),S=x,C=r(c.uBlur,f.uBlur),w=C,T=r(c.uCornerRadius,f.uCornerRadius),E=T,D=n.quality.medium,O=!0,k=!1;function A(e={}){Object.prototype.hasOwnProperty.call(e,`uElementRect`)&&(p=a(e.uElementRect,p)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(h=o(e.uTint,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,f.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(y=r(e.uSpeed,f.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uRefraction`)&&(x=r(e.uRefraction,f.uRefraction)),Object.prototype.hasOwnProperty.call(e,`uBlur`)&&(C=r(e.uBlur,f.uBlur)),Object.prototype.hasOwnProperty.call(e,`uCornerRadius`)&&(T=r(e.uCornerRadius,f.uCornerRadius))}function j(e,t){D=n.quality[e]||n.quality.medium,O=!!t}function M(){}function N(e){for(let t=0;t<4;t+=1)m[t]=s(m[t],p[t],e);for(let t=0;t<3;t+=1)g[t]=s(g[t],h[t],e);v=s(v,_,e),b=s(b,y,e),S=s(S,x,e),w=s(w,C,e),E=s(E,T,e)}function P(e){k||!O||(l.useProgram(u.program),i.bindEngineGlobals(u,e),d.rect!==null&&l.uniform4f(d.rect,m[0],m[1],m[2],m[3]),d.tint!==null&&l.uniform3f(d.tint,g[0],g[1],g[2]),d.intensity!==null&&l.uniform1f(d.intensity,v),d.speed!==null&&l.uniform1f(d.speed,b),d.refraction!==null&&l.uniform1f(d.refraction,S),d.blur!==null&&l.uniform1f(d.blur,w),d.cornerRadius!==null&&l.uniform1f(d.cornerRadius,E),d.waveDensity!==null&&l.uniform1f(d.waveDensity,D.waveDensity),d.opacityCap!==null&&l.uniform1f(d.opacityCap,D.opacityCap),l.bindVertexArray(i.fullscreenVao),l.drawArrays(l.TRIANGLES,0,3))}function F(){k||(k=!0,i.disposeProgram(u))}return{setParameters:A,setQuality:j,resize:M,simulate:N,render:P,dispose:F}}function l(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-glass-refraction-fallback`,r=e.getAttribute(n);e.setAttribute(n,t.state||`active`);let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{l as applyDomFallback,c as createEffect,n as manifest};