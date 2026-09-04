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
uniform vec3 uFromColor;
uniform vec3 uToColor;
uniform vec3 uTint;
uniform float uIntensity;
uniform float uSpeed;
uniform float uDensity;
uniform float uScale;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(91.7, 47.2));
  return fract(sin(dot(p, vec2(19.3, 71.1))) * 12345.6);
}

void main() {
  vec2 centred = vUv - 0.5;
  float radial = 1.0 - smoothstep(0.12, 0.76, length(centred));
  vec3 routeColour = mix(uFromColor, uToColor, uProgress);
  float cells = mix(4.0, 24.0, uDensity) * uScale * mix(0.6, 1.0, uQuality);
  float grain = (hash21(floor(vUv * cells + uTime * uSpeed * 0.02)) - 0.5) * 0.12;
  vec3 colour = mix(routeColour, uTint, radial * 0.5) + uAtmoGlow * (0.08 + radial * 0.16) + grain;
  float pulse = 0.84 + 0.16 * sin(uProgress * 6.283 + uTime * uSpeed * 2.0);
  float alpha = (0.24 + radial * 0.34) * pulse * uIntensity;
  fragColor = vec4(colour, alpha);
}
`,r=Object.freeze({durationMs:200,durationToken:`--pool-motion-normal`,easing:`cubic-bezier(.2, .8, .2, 1)`,easingToken:`--pool-ease-standard`,reducedMotion:`skip-to-end`}),i=Object.freeze({schemaVersion:1,id:`route-crossfade`,title:`Route crossfade`,composition:Object.freeze({slot:`interaction`,blend:`alpha`,zIndex:44,priority:44,exclusiveGroup:`route-transition`}),transition:r,contract:Object.freeze({contentCrossfadeOwner:`DOM-route-coordinator`,shaderRole:`palette-veil`}),uniforms:Object.freeze({uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,description:`Route transition progress.`}),uFromColor:Object.freeze({type:`vec3`,default:[.08,.16,.28],description:`Previous route palette sample.`}),uToColor:Object.freeze({type:`vec3`,default:[.16,.4,.58],description:`Next route palette sample.`}),uTint:Object.freeze({type:`vec3`,default:[.42,.78,.96],description:`Crossfade glow.`}),uIntensity:Object.freeze({type:`float`,default:.22,min:0,max:.48,description:`Veil opacity; DOM remains readable above it.`}),uSpeed:Object.freeze({type:`float`,default:.5,min:0,max:2,description:`Subtle veil motion.`}),uDensity:Object.freeze({type:`float`,default:.46,min:.2,max:1,description:`Veil grain density.`}),uScale:Object.freeze({type:`float`,default:1,min:.65,max:1.8,description:`Veil scale.`})}),quality:Object.freeze({high:Object.freeze({grainFactor:1,alphaCap:1}),medium:Object.freeze({grainFactor:.7,alphaCap:.78}),low:Object.freeze({grainFactor:.45,alphaCap:.52})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.92,medium:.6,low:.28}),gpuMilliseconds:Object.freeze({high:.2,medium:.13,low:.07}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function a(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function o(t,n){let r=Array.isArray(t)||ArrayBuffer.isView(t)?t:n;return[0,1,2].map(t=>e(r[t],n[t]))}function s(e,t,n){let r=1-.001**(Math.max(0,n)/.2);return e+(t-e)*r}function c(){return typeof globalThis.matchMedia==`function`&&globalThis.matchMedia(`(prefers-reduced-motion: reduce)`).matches}function l(r,l={}){let u=r.gl,d=r.createProgram({vertexSource:t,fragmentSource:n,label:i.id}),f=Object.freeze({progress:u.getUniformLocation(d.program,`uProgress`),fromColor:u.getUniformLocation(d.program,`uFromColor`),toColor:u.getUniformLocation(d.program,`uToColor`),tint:u.getUniformLocation(d.program,`uTint`),intensity:u.getUniformLocation(d.program,`uIntensity`),speed:u.getUniformLocation(d.program,`uSpeed`),density:u.getUniformLocation(d.program,`uDensity`),scale:u.getUniformLocation(d.program,`uScale`)}),p=i.uniforms,m=a(l.uProgress,p.uProgress),h=m,g=o(l.uFromColor,p.uFromColor.default),_=g.slice(),v=o(l.uToColor,p.uToColor.default),y=v.slice(),b=o(l.uTint,p.uTint.default),x=b.slice(),S=a(l.uIntensity,p.uIntensity),C=S,w=a(l.uSpeed,p.uSpeed),T=w,E=a(l.uDensity,p.uDensity),D=E,O=a(l.uScale,p.uScale),k=O,A=i.quality.medium,j=c(),M=!j,N=!1;function P(e={}){Object.prototype.hasOwnProperty.call(e,`uProgress`)&&(m=a(e.uProgress,p.uProgress)),Object.prototype.hasOwnProperty.call(e,`uFromColor`)&&(g=o(e.uFromColor,g)),Object.prototype.hasOwnProperty.call(e,`uToColor`)&&(v=o(e.uToColor,v)),Object.prototype.hasOwnProperty.call(e,`uTint`)&&(b=o(e.uTint,b)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(S=a(e.uIntensity,p.uIntensity)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(w=a(e.uSpeed,p.uSpeed)),Object.prototype.hasOwnProperty.call(e,`uDensity`)&&(E=a(e.uDensity,p.uDensity)),Object.prototype.hasOwnProperty.call(e,`uScale`)&&(O=a(e.uScale,p.uScale))}function F(e,t){A=i.quality[e]||i.quality.medium,M=!!t&&!j}function I(){}function L(e){if(j){h=1;return}h=s(h,m,e);for(let t=0;t<3;t+=1)_[t]=s(_[t],g[t],e),y[t]=s(y[t],v[t],e),x[t]=s(x[t],b[t],e);C=s(C,S,e),T=s(T,w,e),D=s(D,E,e),k=s(k,O,e)}function R(t){N||!M||h<=.001||h>=.999||(u.useProgram(d.program),r.bindEngineGlobals(d,t),f.progress!==null&&u.uniform1f(f.progress,e(h)),f.fromColor!==null&&u.uniform3fv(f.fromColor,_),f.toColor!==null&&u.uniform3fv(f.toColor,y),f.tint!==null&&u.uniform3fv(f.tint,x),f.intensity!==null&&u.uniform1f(f.intensity,C*A.alphaCap),f.speed!==null&&u.uniform1f(f.speed,T),f.density!==null&&u.uniform1f(f.density,D*A.grainFactor),f.scale!==null&&u.uniform1f(f.scale,k),u.bindVertexArray(r.fullscreenVao),u.drawArrays(u.TRIANGLES,0,3))}function z(){N||(N=!0,r.disposeProgram(d))}return{setParameters:P,setQuality:F,resize:I,simulate:L,render:R,dispose:z}}function u(e,t={}){if(!e||typeof e.setAttribute!=`function`)return()=>{};let n=`data-aurora-route-crossfade-fallback`,r=e.getAttribute(n);e.setAttribute(n,String(t.reason||`dom-crossfade`));let i=!1;return()=>{i||(i=!0,r===null?e.removeAttribute(n):e.setAttribute(n,r))}}export{u as applyDomFallback,l as createEffect,i as manifest,r as transition};