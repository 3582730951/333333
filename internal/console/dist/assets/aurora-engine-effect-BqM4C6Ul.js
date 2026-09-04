import"./aurora-engine-contracts-CjO_kDw4.js";var e=`#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,t=`#version 300 es
precision mediump float;

uniform float uTime;
uniform vec2 uResolution;
uniform vec3 uAtmoGlow;
uniform vec3 uAtmoFar;
uniform vec2 uPointer;
uniform float uRadius;
uniform float uIntensity;
uniform float uTrail;

in vec2 vUv;
out vec4 fragColor;

// Two-lobe halo: a tight core that reads as "the cursor is here" and a wide,
// slower lobe that reads as "it came from over there". The trail is faked in the
// falloff rather than by keeping history -- steady-state zero allocation (R3).
void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  float d = length((vUv - uPointer) * aspect);
  float core = 1.0 - smoothstep(0.0, max(uRadius, 0.0001), d);
  float wide = 1.0 - smoothstep(0.0, max(uRadius * 3.0, 0.0001), d);
  float pulse = 0.94 + 0.06 * sin(uTime * 3.4);
  float alpha = (core * 0.75 + wide * uTrail * 0.35) * uIntensity * pulse;
  vec3 tint = mix(uAtmoFar, uAtmoGlow, core);
  fragColor = vec4(tint * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`cursor-glow`,title:`Cursor Glow`,composition:Object.freeze({slot:`interaction`,blend:`additive`,zIndex:41,priority:41,exclusiveGroup:`pointer-affordance`}),uniforms:Object.freeze({uPointer:Object.freeze({type:`vec2`,default:[.5,.5],min:0,max:1,step:.001,description:`Pointer position in effect UV space.`}),uRadius:Object.freeze({type:`float`,default:.09,min:.02,max:.4,step:.005,description:`Core halo radius.`}),uIntensity:Object.freeze({type:`float`,default:.42,min:0,max:1,step:.01,description:`Halo opacity.`}),uTrail:Object.freeze({type:`float`,default:.55,min:0,max:1,step:.01,description:`Weight of the wide trailing lobe.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,alphaCap:.85}),low:Object.freeze({renderScale:.5,alphaCap:.6})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.3,medium:.2,low:.12}),gpuMilliseconds:Object.freeze({high:.11,medium:.07,low:.04}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`main-only`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t){if(!Array.isArray(e)||e.length<2)return t.default;let n=Number(e[0]),r=Number(e[1]);return!Number.isFinite(n)||!Number.isFinite(r)?t.default:[Math.min(t.max,Math.max(t.min,n)),Math.min(t.max,Math.max(t.min,r))]}function a(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function o(o,s={}){let c=o.gl,l=o.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),u=c.getUniformLocation(l.program,`uPointer`),d=c.getUniformLocation(l.program,`uRadius`),f=c.getUniformLocation(l.program,`uIntensity`),p=c.getUniformLocation(l.program,`uTrail`),m=n.uniforms.uPointer,h=n.uniforms.uRadius,g=n.uniforms.uIntensity,_=n.uniforms.uTrail,v=i(s.uPointer,m),y=r(s.uRadius,h),b=r(s.uIntensity,g),x=r(s.uTrail,_),S=v[0],C=v[1],w=y,T=b,E=x,D=n.quality.medium,O=!0,k=!1;function A(e={}){Object.prototype.hasOwnProperty.call(e,`uPointer`)&&(v=i(e.uPointer,m)),Object.prototype.hasOwnProperty.call(e,`uRadius`)&&(y=r(e.uRadius,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(b=r(e.uIntensity,g)),Object.prototype.hasOwnProperty.call(e,`uTrail`)&&(x=r(e.uTrail,_))}function j(e,t){D=n.quality[e]||n.quality.medium,O=!!t}function M(){}function N(e){S=a(S,v[0],e,20),C=a(C,v[1],e,20),w=a(w,y,e,6),T=a(T,b,e,6),E=a(E,x,e,6)}function P(e){k||!O||(c.useProgram(l.program),o.bindEngineGlobals(l,e),u!==null&&c.uniform2f(u,S,C),d!==null&&c.uniform1f(d,w),f!==null&&c.uniform1f(f,T*D.alphaCap),p!==null&&c.uniform1f(p,E),c.bindVertexArray(o.fullscreenVao),c.drawArrays(c.TRIANGLES,0,3))}function F(){k||(k=!0,o.disposeProgram(l))}return{setParameters:A,setQuality:j,resize:M,simulate:N,render:P,dispose:F}}function s(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-cursor-glow-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{s as applyDomFallback,o as createEffect,n as manifest};