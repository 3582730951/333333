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
uniform vec3 uAtmoGlow;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uStripeScale;
uniform float uIridescence;
uniform float uNoise;
uniform float uCornerRadius;
uniform float uDetail;
uniform float uAlphaCap;

out vec4 fragColor;

float hash12(vec2 point) {
  return fract(sin(dot(point, vec2(41.7, 113.3))) * 157.31);
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
  float stripe = 0.5 + 0.5 * sin((pixel.x + pixel.y * 0.46) / max(1.0, uStripeScale * dpr) + uTime * uSpeed);
  float colorPhase = stripe * 6.28318 * uIridescence + uTime * uSpeed * 0.35;
  float sparkle = hash12(floor(pixel / max(1.0, dpr * 2.0)) + floor(uTime * 2.0));
  vec3 first = mix(uAtmoNear, uTint, 0.3 + 0.7 * stripe);
  vec3 second = mix(uAtmoGlow, uAtmoFar, 0.35 + 0.55 * (1.0 - stripe));
  vec3 color = mix(first, second, 0.5 + 0.5 * sin(colorPhase));
  color += (sparkle - 0.5) * uNoise * uDetail;
  float edge = pow(1.0 - clamp(length(point) * 1.4, 0.0, 1.0), 2.0);
  float alpha = shape * uIntensity * uAlphaCap * (0.34 + stripe * 0.34 + edge * 0.32);
  fragColor = vec4(max(color, vec3(0.0)), alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`holographic`,title:`Holographic`,composition:Object.freeze({slot:`foreground`,blend:`alpha`,zIndex:43,priority:43,exclusiveGroup:`material-surface`}),uniforms:Object.freeze({uElementRect:Object.freeze({type:`vec4`,default:[.25,.25,.5,.5],description:`Normalized [left, bottom, width, height] target rect.`}),uTint:Object.freeze({type:`vec3`,default:[.6,.95,1],description:`Holographic base tint.`}),uIntensity:Object.freeze({type:`float`,default:.24,min:0,max:.7,step:.01,description:`Iridescent overlay opacity.`}),uSpeed:Object.freeze({type:`float`,default:.42,min:-2,max:2,step:.01,description:`Fixed-clock color sweep speed.`}),uStripeScale:Object.freeze({type:`float`,default:18,min:2,max:64,step:.5,description:`Holographic stripe size in CSS pixels before DPR scaling.`}),uIridescence:Object.freeze({type:`float`,default:.72,min:0,max:2,step:.01,description:`Palette phase separation.`}),uNoise:Object.freeze({type:`float`,default:.08,min:0,max:.3,step:.01,description:`Sparse sparkle amount.`}),uCornerRadius:Object.freeze({type:`float`,default:.1,min:0,max:.3,step:.01,description:`Rounded surface mask.`})}),quality:Object.freeze({high:Object.freeze({detail:1,alphaCap:1}),medium:Object.freeze({detail:.72,alphaCap:.78}),low:Object.freeze({detail:.42,alphaCap:.5})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.5,medium:.32,low:.13}),gpuMilliseconds:Object.freeze({high:.14,medium:.09,low:.045}),fill:`partial`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r,i){let a=Number(e&&e[t]);return Number.isFinite(a)?Math.min(i,Math.max(r,a)):n[t]}function a(e,t){return[i(e,0,t,-.5,1.5),i(e,1,t,-.5,1.5),i(e,2,t,.001,2),i(e,3,t,.001,2)]}function o(e,t){return[i(e,0,t,0,1),i(e,1,t,0,1),i(e,2,t,0,1)]}function s(e,t,n){return e+(t-e)*(1-1e-4**Math.max(0,n))}function c(i,c={}){let l=i.gl,u=i.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),d=Object.freeze({rect:l.getUniformLocation(u.program,`uElementRect`),tint:l.getUniformLocation(u.program,`uTint`),intensity:l.getUniformLocation(u.program,`uIntensity`),speed:l.getUniformLocation(u.program,`uSpeed`),stripeScale:l.getUniformLocation(u.program,`uStripeScale`),iridescence:l.getUniformLocation(u.program,`uIridescence`),noise:l.getUniformLocation(u.program,`uNoise`),cornerRadius:l.getUniformLocation(u.program,`uCornerRadius`),detail:l.getUniformLocation(u.program,`uDetail`),alphaCap:l.getUniformLocation(u.program,`uAlphaCap`)}),f=n.uniforms,p=a(c.uElementRect,f.uElementRect.default),m=p.slice(),h=o(c.uTint,f.uTint.default),g=h.slice(),_=r(c.uIntensity,f.uIntensity),v=_,y=r(c.uSpeed,f.uSpeed),b=y,x=r(c.uStripeScale,f.uStripeScale),S=x,C=r(c.uIridescence,f.uIridescence),w=C,T=r(c.uNoise,f.uNoise),E=T,D=r(c.uCornerRadius,f.uCornerRadius),O=D,k=n.quality.medium,A=!0,j=!1;function M(e={}){Object.prototype.hasOwnProperty.call(e,`uElementRect`)&&(p=a(e.uElementRect,p)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(h=o(e.uTint,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,f.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(y=r(e.uSpeed,f.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uStripeScale`)&&(x=r(e.uStripeScale,f.uStripeScale)),Object.prototype.hasOwnProperty.call(e,`uIridescence`)&&(C=r(e.uIridescence,f.uIridescence)),Object.prototype.hasOwnProperty.call(e,`uNoise`)&&(T=r(e.uNoise,f.uNoise)),Object.prototype.hasOwnProperty.call(e,`uCornerRadius`)&&(D=r(e.uCornerRadius,f.uCornerRadius))}function N(e,t){k=n.quality[e]||n.quality.medium,A=!!t}function P(){}function F(e){for(let t=0;t<4;t+=1)m[t]=s(m[t],p[t],e);for(let t=0;t<3;t+=1)g[t]=s(g[t],h[t],e);v=s(v,_,e),b=s(b,y,e),S=s(S,x,e),w=s(w,C,e),E=s(E,T,e),O=s(O,D,e)}function I(e){j||!A||(l.useProgram(u.program),i.bindEngineGlobals(u,e),d.rect!==null&&l.uniform4f(d.rect,m[0],m[1],m[2],m[3]),d.tint!==null&&l.uniform3f(d.tint,g[0],g[1],g[2]),d.intensity!==null&&l.uniform1f(d.intensity,v),d.speed!==null&&l.uniform1f(d.speed,b),d.stripeScale!==null&&l.uniform1f(d.stripeScale,S),d.iridescence!==null&&l.uniform1f(d.iridescence,w),d.noise!==null&&l.uniform1f(d.noise,E),d.cornerRadius!==null&&l.uniform1f(d.cornerRadius,O),d.detail!==null&&l.uniform1f(d.detail,k.detail),d.alphaCap!==null&&l.uniform1f(d.alphaCap,k.alphaCap),l.bindVertexArray(i.fullscreenVao),l.drawArrays(l.TRIANGLES,0,3))}function L(){j||(j=!0,i.disposeProgram(u))}return{setParameters:M,setQuality:N,resize:P,simulate:F,render:I,dispose:L}}function l(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-holographic-fallback`,r=e.getAttribute(n);e.setAttribute(n,t.state||`active`);let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{l as applyDomFallback,c as createEffect,n as manifest};