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
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;
uniform float uProgress;
uniform vec2 uDirection;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
uniform float uSoftness;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(127.1, 311.7));
  return fract(sin(dot(p, vec2(41.3, 17.7))) * 4375.85);
}

void main() {
  vec2 direction = normalize(uDirection + vec2(0.0001));
  vec2 centred = vUv - 0.5;
  float directional = dot(centred, direction) + 0.5;
  float field = mix(dot(mix(uAtmoNear, uAtmoFar, vUv.y), vec3(0.22, 0.7, 0.08)), directional, 0.64);
  float cells = mix(5.0, 28.0, uDensity) * uScale * mix(0.6, 1.0, uQuality);
  float grain = (hash21(floor(vUv * cells + vec2(uTime * uSpeed * 0.03))) - 0.5) * 0.14;
  float luminance = clamp(field + grain, 0.0, 1.0);
  float threshold = uProgress;
  float wipe = smoothstep(threshold - uSoftness, threshold + uSoftness, luminance);
  float edge = 1.0 - smoothstep(0.0, uSoftness * 2.8, abs(luminance - threshold));
  vec3 color = mix(uAtmoFar, uTint, 0.76) + uAtmoGlow * edge * 0.2;
  float alpha = (1.0 - wipe) * 0.26 * uIntensity + edge * 0.74 * uIntensity;
  fragColor = vec4(color, alpha * (0.55 + 0.45 * uQuality));
}
`,r=Object.freeze({durationMs:200,durationToken:`--pool-motion-normal`,easing:`cubic-bezier(0, 0, .2, 1)`,easingToken:`--pool-ease-enter`,reducedMotion:`skip-to-end`}),i=Object.freeze({schemaVersion:1,id:`luminance-wipe`,title:`Luminance wipe transition`,composition:Object.freeze({slot:`interaction`,blend:`alpha`,zIndex:43,priority:43,exclusiveGroup:`route-transition`}),transition:r,contract:Object.freeze({sourceTexture:`palette-procedural`,domContentOwner:!0}),uniforms:Object.freeze({uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,description:`Luminance threshold progress.`}),uDirection:Object.freeze({type:`vec2`,default:[1,0],description:`Normalized wipe direction.`}),uTint:Object.freeze({type:`vec3`,default:[.58,.82,1],description:`Wipe edge tint.`}),uIntensity:Object.freeze({type:`float`,default:.3,min:0,max:.62,description:`Wipe opacity.`}),uSpeed:Object.freeze({type:`float`,default:.7,min:0,max:2,description:`Luminance-field drift speed.`}),uDensity:Object.freeze({type:`float`,default:.5,min:.2,max:1,description:`Field detail density.`}),uScale:Object.freeze({type:`float`,default:1,min:.65,max:1.8,description:`Field scale.`}),uSoftness:Object.freeze({type:`float`,default:.08,min:.02,max:.2,description:`Threshold feather.`})}),quality:Object.freeze({high:Object.freeze({fieldScale:1,alphaCap:1}),medium:Object.freeze({fieldScale:.74,alphaCap:.8}),low:Object.freeze({fieldScale:.5,alphaCap:.56})}),cost:Object.freeze({budgetUnits:Object.freeze({high:1.05,medium:.68,low:.32}),gpuMilliseconds:Object.freeze({high:.23,medium:.15,low:.08}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function a(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function o(e,t,n){let r=Array.isArray(e)||ArrayBuffer.isView(e)?e:t,i=[];for(let e=0;e<n;e+=1)i.push(Number.isFinite(Number(r[e]))?Number(r[e]):Number(t[e]));return i}function s(e,t,n){let r=1-.001**(Math.max(0,n)/.2);return e+(t-e)*r}function c(){return typeof globalThis.matchMedia==`function`&&globalThis.matchMedia(`(prefers-reduced-motion: reduce)`).matches}function l(r,l={}){let u=r.gl,d=r.createProgram({vertexSource:t,fragmentSource:n,label:i.id}),f=Object.freeze({progress:u.getUniformLocation(d.program,`uProgress`),direction:u.getUniformLocation(d.program,`uDirection`),tint:u.getUniformLocation(d.program,`uTint`),intensity:u.getUniformLocation(d.program,`uIntensity`),speed:u.getUniformLocation(d.program,`uSpeed`),density:u.getUniformLocation(d.program,`uDensity`),scale:u.getUniformLocation(d.program,`uScale`),softness:u.getUniformLocation(d.program,`uSoftness`)}),p=i.uniforms,m=a(l.uProgress,p.uProgress),h=m,g=o(l.uDirection,p.uDirection.default,2),_=g.slice(),v=o(l.uTint,p.uTint.default,3).map(t=>e(t)),y=v.slice(),b=a(l.uIntensity,p.uIntensity),x=b,S=a(l.uSpeed,p.uSpeed),C=S,w=a(l.uDensity,p.uDensity),T=w,E=a(l.uScale,p.uScale),D=E,O=a(l.uSoftness,p.uSoftness),k=O,A=i.quality.medium,j=c(),M=!j,N=!1;function P(t={}){Object.prototype.hasOwnProperty.call(t,`uProgress`)&&(m=a(t.uProgress,p.uProgress)),Object.prototype.hasOwnProperty.call(t,`uDirection`)&&(g=o(t.uDirection,g,2)),Object.prototype.hasOwnProperty.call(t,`uTint`)&&(v=o(t.uTint,v,3).map(t=>e(t))),Object.prototype.hasOwnProperty.call(t,`uIntensity`)&&(b=a(t.uIntensity,p.uIntensity)),Object.prototype.hasOwnProperty.call(t,`uSpeed`)&&(S=a(t.uSpeed,p.uSpeed)),Object.prototype.hasOwnProperty.call(t,`uDensity`)&&(w=a(t.uDensity,p.uDensity)),Object.prototype.hasOwnProperty.call(t,`uScale`)&&(E=a(t.uScale,p.uScale)),Object.prototype.hasOwnProperty.call(t,`uSoftness`)&&(O=a(t.uSoftness,p.uSoftness))}function F(e,t){A=i.quality[e]||i.quality.medium,M=!!t&&!j}function I(){}function L(e){if(j){h=1;return}h=s(h,m,e);for(let t=0;t<2;t+=1)_[t]=s(_[t],g[t],e);for(let t=0;t<3;t+=1)y[t]=s(y[t],v[t],e);x=s(x,b,e),C=s(C,S,e),T=s(T,w,e),D=s(D,E,e),k=s(k,O,e)}function R(t){N||!M||h<=.001||h>=.999||(u.useProgram(d.program),r.bindEngineGlobals(d,t),f.progress!==null&&u.uniform1f(f.progress,e(h)),f.direction!==null&&u.uniform2fv(f.direction,_),f.tint!==null&&u.uniform3fv(f.tint,y),f.intensity!==null&&u.uniform1f(f.intensity,x*A.alphaCap),f.speed!==null&&u.uniform1f(f.speed,C),f.density!==null&&u.uniform1f(f.density,T*A.fieldScale),f.scale!==null&&u.uniform1f(f.scale,D),f.softness!==null&&u.uniform1f(f.softness,k),u.bindVertexArray(r.fullscreenVao),u.drawArrays(u.TRIANGLES,0,3))}function z(){N||(N=!0,r.disposeProgram(d))}return{setParameters:P,setQuality:F,resize:I,simulate:L,render:R,dispose:z}}function u(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-luminance-wipe-fallback`,r=e.getAttribute(n);e.setAttribute(n,String(t.state||`true`));let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{u as applyDomFallback,l as createEffect,i as manifest,r as transition};