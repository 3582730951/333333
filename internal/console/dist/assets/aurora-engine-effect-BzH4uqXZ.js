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
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;

uniform vec4 uElementRect;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uLineSpacing;
uniform float uSpeed;
uniform float uThickness;
uniform float uSoftness;
uniform float uSkew;
uniform float uLineDensity;
uniform float uContrast;
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
  float shape = 1.0 - smoothstep(0.0, 0.012, roundedBox(point, vec2(0.5 * aspect, 0.5), 0.08));
  vec2 physical = max(uElementRect.zw * uResolution, vec2(1.0));
  vec2 pixel = vUv * physical;
  float spacing = max(1.0, uLineSpacing * max(uPixelRatio, 1.0));
  float phase = fract((pixel.y + pixel.x * uSkew) / spacing * uLineDensity + uTime * uSpeed);
  float distanceToLine = abs(phase - 0.5);
  float line = 1.0 - smoothstep(uThickness, uThickness + uSoftness, distanceToLine);
  float shimmer = 0.5 + 0.5 * sin(vUv.x * 9.0 + uTime * uSpeed * 0.7);
  vec3 color = mix(uAtmoFar, uAtmoGlow, line * (0.55 + shimmer * 0.35));
  color = mix(color, uTint, 0.26 + line * 0.3);
  float alpha = shape * uIntensity * uAlphaCap * (0.18 + line * uContrast);
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`scanline`,title:`Scanline`,composition:Object.freeze({slot:`foreground`,blend:`alpha`,zIndex:41,priority:41,exclusiveGroup:`material-surface`}),uniforms:Object.freeze({uElementRect:Object.freeze({type:`vec4`,default:[.25,.25,.5,.5],description:`Normalized [left, bottom, width, height] target rect.`}),uTint:Object.freeze({type:`vec3`,default:[.45,.82,1],description:`Scanline tint.`}),uIntensity:Object.freeze({type:`float`,default:.2,min:0,max:.6,step:.01,description:`Overlay opacity.`}),uLineSpacing:Object.freeze({type:`float`,default:4,min:1,max:24,step:.25,description:`Line spacing in CSS pixels before DPR scaling.`}),uSpeed:Object.freeze({type:`float`,default:.24,min:-2,max:2,step:.01,description:`Fixed-clock vertical travel.`}),uThickness:Object.freeze({type:`float`,default:.22,min:.03,max:.46,step:.01,description:`Line duty cycle.`}),uSoftness:Object.freeze({type:`float`,default:.08,min:.01,max:.3,step:.01,description:`Line edge feather.`}),uSkew:Object.freeze({type:`float`,default:.08,min:-.8,max:.8,step:.01,description:`Diagonal scanline skew.`})}),quality:Object.freeze({high:Object.freeze({lineDensity:1,contrast:1,alphaCap:1}),medium:Object.freeze({lineDensity:.78,contrast:.78,alphaCap:.76}),low:Object.freeze({lineDensity:.58,contrast:.56,alphaCap:.5})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.3,medium:.19,low:.08}),gpuMilliseconds:Object.freeze({high:.085,medium:.055,low:.03}),fill:`partial`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r,i){let a=Number(e&&e[t]);return Number.isFinite(a)?Math.min(i,Math.max(r,a)):n[t]}function a(e,t){return[i(e,0,t,-.5,1.5),i(e,1,t,-.5,1.5),i(e,2,t,.001,2),i(e,3,t,.001,2)]}function o(e,t){return[i(e,0,t,0,1),i(e,1,t,0,1),i(e,2,t,0,1)]}function s(e,t,n){return e+(t-e)*(1-1e-4**Math.max(0,n))}function c(i,c={}){let l=i.gl,u=i.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),d=Object.freeze({rect:l.getUniformLocation(u.program,`uElementRect`),tint:l.getUniformLocation(u.program,`uTint`),intensity:l.getUniformLocation(u.program,`uIntensity`),lineSpacing:l.getUniformLocation(u.program,`uLineSpacing`),speed:l.getUniformLocation(u.program,`uSpeed`),thickness:l.getUniformLocation(u.program,`uThickness`),softness:l.getUniformLocation(u.program,`uSoftness`),skew:l.getUniformLocation(u.program,`uSkew`),lineDensity:l.getUniformLocation(u.program,`uLineDensity`),contrast:l.getUniformLocation(u.program,`uContrast`),alphaCap:l.getUniformLocation(u.program,`uAlphaCap`)}),f=n.uniforms,p=a(c.uElementRect,f.uElementRect.default),m=p.slice(),h=o(c.uTint,f.uTint.default),g=h.slice(),_=r(c.uIntensity,f.uIntensity),v=_,y=r(c.uLineSpacing,f.uLineSpacing),b=y,x=r(c.uSpeed,f.uSpeed),S=x,C=r(c.uThickness,f.uThickness),w=C,T=r(c.uSoftness,f.uSoftness),E=T,D=r(c.uSkew,f.uSkew),O=D,k=n.quality.medium,A=!0,j=!1;function M(e={}){Object.prototype.hasOwnProperty.call(e,`uElementRect`)&&(p=a(e.uElementRect,p)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(h=o(e.uTint,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,f.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uLineSpacing`)&&(y=r(e.uLineSpacing,f.uLineSpacing)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(x=r(e.uSpeed,f.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uThickness`)&&(C=r(e.uThickness,f.uThickness)),Object.prototype.hasOwnProperty.call(e,`uSoftness`)&&(T=r(e.uSoftness,f.uSoftness)),Object.prototype.hasOwnProperty.call(e,`uSkew`)&&(D=r(e.uSkew,f.uSkew))}function N(e,t){k=n.quality[e]||n.quality.medium,A=!!t}function P(){}function F(e){for(let t=0;t<4;t+=1)m[t]=s(m[t],p[t],e);for(let t=0;t<3;t+=1)g[t]=s(g[t],h[t],e);v=s(v,_,e),b=s(b,y,e),S=s(S,x,e),w=s(w,C,e),E=s(E,T,e),O=s(O,D,e)}function I(e){j||!A||(l.useProgram(u.program),i.bindEngineGlobals(u,e),d.rect!==null&&l.uniform4f(d.rect,m[0],m[1],m[2],m[3]),d.tint!==null&&l.uniform3f(d.tint,g[0],g[1],g[2]),d.intensity!==null&&l.uniform1f(d.intensity,v),d.lineSpacing!==null&&l.uniform1f(d.lineSpacing,b),d.speed!==null&&l.uniform1f(d.speed,S),d.thickness!==null&&l.uniform1f(d.thickness,w),d.softness!==null&&l.uniform1f(d.softness,E),d.skew!==null&&l.uniform1f(d.skew,O),d.lineDensity!==null&&l.uniform1f(d.lineDensity,k.lineDensity),d.contrast!==null&&l.uniform1f(d.contrast,k.contrast),d.alphaCap!==null&&l.uniform1f(d.alphaCap,k.alphaCap),l.bindVertexArray(i.fullscreenVao),l.drawArrays(l.TRIANGLES,0,3))}function L(){j||(j=!0,i.disposeProgram(u))}return{setParameters:M,setQuality:N,resize:P,simulate:F,render:I,dispose:L}}function l(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-scanline-fallback`,r=e.getAttribute(n);e.setAttribute(n,t.state||`active`);let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{l as applyDomFallback,c as createEffect,n as manifest};