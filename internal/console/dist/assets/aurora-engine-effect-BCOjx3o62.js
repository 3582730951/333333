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
uniform vec3 uAtmoNear;
uniform vec3 uAtmoGlow;
uniform vec3 uAtmoFar;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uWaveScale;
uniform float uThickness;
uniform float uSoftness;
uniform float uOffset;
uniform float uWaveDetail;
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
  float shape = 1.0 - smoothstep(0.0, 0.012, roundedBox(point, vec2(0.5 * aspect, 0.5), 0.1));
  float phase = uTime * uSpeed;
  float wave = sin(vUv.x * uWaveScale + phase) * 0.055 * uWaveDetail;
  wave += sin(vUv.x * uWaveScale * 0.47 - phase * 0.72) * 0.035 * uWaveDetail;
  float centre = clamp(0.75 + uOffset + wave, 0.12, 0.94);
  float distanceToRidge = abs(vUv.y - centre);
  float ridge = 1.0 - smoothstep(uThickness, uThickness + uSoftness, distanceToRidge);
  float secondary = 1.0 - smoothstep(uThickness * 0.65, uThickness + uSoftness * 1.7, abs(vUv.y - centre - 0.065));
  float topMask = smoothstep(0.28, 0.72, vUv.y);
  float flow = 0.5 + 0.5 * sin(vUv.x * 7.0 - phase * 0.5);
  vec3 color = mix(uAtmoNear, uAtmoGlow, flow * 0.5 + ridge * 0.5);
  color = mix(color, uTint, 0.38 + ridge * 0.4);
  float alpha = shape * topMask * uIntensity * uAlphaCap * (ridge * 0.9 + secondary * 0.3);
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`liquid-highlight`,title:`Liquid Highlight`,composition:Object.freeze({slot:`foreground`,blend:`alpha`,zIndex:45,priority:45,exclusiveGroup:`material-surface`}),uniforms:Object.freeze({uElementRect:Object.freeze({type:`vec4`,default:[.25,.25,.5,.5],description:`Normalized [left, bottom, width, height] target rect.`}),uTint:Object.freeze({type:`vec3`,default:[.55,.92,1],description:`Liquid highlight tint.`}),uIntensity:Object.freeze({type:`float`,default:.3,min:0,max:.8,step:.01,description:`Highlight opacity.`}),uSpeed:Object.freeze({type:`float`,default:.5,min:-2,max:2,step:.01,description:`Fixed-clock wave speed.`}),uWaveScale:Object.freeze({type:`float`,default:10,min:2,max:36,step:.25,description:`Horizontal wave frequency.`}),uThickness:Object.freeze({type:`float`,default:.035,min:.005,max:.16,step:.005,description:`Ridge thickness.`}),uSoftness:Object.freeze({type:`float`,default:.045,min:.005,max:.2,step:.005,description:`Ridge feather.`}),uOffset:Object.freeze({type:`float`,default:0,min:-.35,max:.2,step:.01,description:`Vertical ridge offset.`})}),quality:Object.freeze({high:Object.freeze({waveDetail:1,alphaCap:1}),medium:Object.freeze({waveDetail:.72,alphaCap:.78}),low:Object.freeze({waveDetail:.42,alphaCap:.52})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.42,medium:.27,low:.11}),gpuMilliseconds:Object.freeze({high:.12,medium:.075,low:.04}),fill:`partial`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r,i){let a=Number(e&&e[t]);return Number.isFinite(a)?Math.min(i,Math.max(r,a)):n[t]}function a(e,t){return[i(e,0,t,-.5,1.5),i(e,1,t,-.5,1.5),i(e,2,t,.001,2),i(e,3,t,.001,2)]}function o(e,t){return[i(e,0,t,0,1),i(e,1,t,0,1),i(e,2,t,0,1)]}function s(e,t,n){return e+(t-e)*(1-1e-4**Math.max(0,n))}function c(i,c={}){let l=i.gl,u=i.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),d=Object.freeze({rect:l.getUniformLocation(u.program,`uElementRect`),tint:l.getUniformLocation(u.program,`uTint`),intensity:l.getUniformLocation(u.program,`uIntensity`),speed:l.getUniformLocation(u.program,`uSpeed`),waveScale:l.getUniformLocation(u.program,`uWaveScale`),thickness:l.getUniformLocation(u.program,`uThickness`),softness:l.getUniformLocation(u.program,`uSoftness`),offset:l.getUniformLocation(u.program,`uOffset`),waveDetail:l.getUniformLocation(u.program,`uWaveDetail`),alphaCap:l.getUniformLocation(u.program,`uAlphaCap`)}),f=n.uniforms,p=a(c.uElementRect,f.uElementRect.default),m=p.slice(),h=o(c.uTint,f.uTint.default),g=h.slice(),_=r(c.uIntensity,f.uIntensity),v=_,y=r(c.uSpeed,f.uSpeed),b=y,x=r(c.uWaveScale,f.uWaveScale),S=x,C=r(c.uThickness,f.uThickness),w=C,T=r(c.uSoftness,f.uSoftness),E=T,D=r(c.uOffset,f.uOffset),O=D,k=n.quality.medium,A=!0,j=!1;function M(e={}){Object.prototype.hasOwnProperty.call(e,`uElementRect`)&&(p=a(e.uElementRect,p)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(h=o(e.uTint,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,f.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(y=r(e.uSpeed,f.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uWaveScale`)&&(x=r(e.uWaveScale,f.uWaveScale)),Object.prototype.hasOwnProperty.call(e,`uThickness`)&&(C=r(e.uThickness,f.uThickness)),Object.prototype.hasOwnProperty.call(e,`uSoftness`)&&(T=r(e.uSoftness,f.uSoftness)),Object.prototype.hasOwnProperty.call(e,`uOffset`)&&(D=r(e.uOffset,f.uOffset))}function N(e,t){k=n.quality[e]||n.quality.medium,A=!!t}function P(){}function F(e){for(let t=0;t<4;t+=1)m[t]=s(m[t],p[t],e);for(let t=0;t<3;t+=1)g[t]=s(g[t],h[t],e);v=s(v,_,e),b=s(b,y,e),S=s(S,x,e),w=s(w,C,e),E=s(E,T,e),O=s(O,D,e)}function I(e){j||!A||(l.useProgram(u.program),i.bindEngineGlobals(u,e),d.rect!==null&&l.uniform4f(d.rect,m[0],m[1],m[2],m[3]),d.tint!==null&&l.uniform3f(d.tint,g[0],g[1],g[2]),d.intensity!==null&&l.uniform1f(d.intensity,v),d.speed!==null&&l.uniform1f(d.speed,b),d.waveScale!==null&&l.uniform1f(d.waveScale,S),d.thickness!==null&&l.uniform1f(d.thickness,w),d.softness!==null&&l.uniform1f(d.softness,E),d.offset!==null&&l.uniform1f(d.offset,O),d.waveDetail!==null&&l.uniform1f(d.waveDetail,k.waveDetail),d.alphaCap!==null&&l.uniform1f(d.alphaCap,k.alphaCap),l.bindVertexArray(i.fullscreenVao),l.drawArrays(l.TRIANGLES,0,3))}function L(){j||(j=!0,i.disposeProgram(u))}return{setParameters:M,setQuality:N,resize:P,simulate:F,render:I,dispose:L}}function l(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-liquid-highlight-fallback`,r=e.getAttribute(n);e.setAttribute(n,t.state||`active`);let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{l as applyDomFallback,c as createEffect,n as manifest};