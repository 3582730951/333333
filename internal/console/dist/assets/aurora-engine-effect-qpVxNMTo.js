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
uniform vec2 uOrigin;
uniform float uPress;
uniform float uSpring;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Elastic press: a compression ring that overshoots once and settles. uPress is
// driven by the host from pointerdown/up, so the visual is same-frame with the
// input (R4) -- the shader never waits on an animation clock of its own.
void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  float d = length((vUv - uOrigin) * aspect);
  float overshoot = sin(uPress * 3.14159) * uSpring;
  float radius = 0.16 + overshoot * 0.09;
  float ring = 1.0 - smoothstep(radius * 0.55, radius, d);
  float rim = smoothstep(radius * 0.72, radius, d) * (1.0 - smoothstep(radius, radius * 1.25, d));
  float alpha = (ring * 0.35 + rim * 0.9) * uPress * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`press-elastic`,title:`Press Elastic`,composition:Object.freeze({slot:`interaction`,blend:`additive`,zIndex:42,priority:42,exclusiveGroup:`pointer-press`}),uniforms:Object.freeze({uOrigin:Object.freeze({type:`vec2`,default:[.5,.5],min:0,max:1,step:.001,description:`Press origin in effect UV space.`}),uPress:Object.freeze({type:`float`,default:0,min:0,max:1,step:.01,description:`Press envelope, 0 released to 1 fully pressed.`}),uSpring:Object.freeze({type:`float`,default:.6,min:0,max:1.5,step:.01,description:`Overshoot amount of the elastic settle.`}),uIntensity:Object.freeze({type:`float`,default:.5,min:0,max:1,step:.01,description:`Ring opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,alphaCap:.85}),low:Object.freeze({renderScale:.5,alphaCap:.6})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.26,medium:.18,low:.1}),gpuMilliseconds:Object.freeze({high:.1,medium:.06,low:.035}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`main-only`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t){if(!Array.isArray(e)||e.length<2)return t.default;let n=Number(e[0]),r=Number(e[1]);return!Number.isFinite(n)||!Number.isFinite(r)?t.default:[Math.min(t.max,Math.max(t.min,n)),Math.min(t.max,Math.max(t.min,r))]}function a(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function o(o,s={}){let c=o.gl,l=o.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),u=c.getUniformLocation(l.program,`uOrigin`),d=c.getUniformLocation(l.program,`uPress`),f=c.getUniformLocation(l.program,`uSpring`),p=c.getUniformLocation(l.program,`uIntensity`),m=n.uniforms.uOrigin,h=n.uniforms.uPress,g=n.uniforms.uSpring,_=n.uniforms.uIntensity,v=i(s.uOrigin,m),y=r(s.uPress,h),b=r(s.uSpring,g),x=r(s.uIntensity,_),S=v[0],C=v[1],w=y,T=b,E=x,D=n.quality.medium,O=!0,k=!1;function A(e={}){Object.prototype.hasOwnProperty.call(e,`uOrigin`)&&(v=i(e.uOrigin,m)),Object.prototype.hasOwnProperty.call(e,`uPress`)&&(y=r(e.uPress,h)),Object.prototype.hasOwnProperty.call(e,`uSpring`)&&(b=r(e.uSpring,g)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(x=r(e.uIntensity,_))}function j(e,t){D=n.quality[e]||n.quality.medium,O=!!t}function M(){}function N(e){S=a(S,v[0],e,24),C=a(C,v[1],e,24),w=a(w,y,e,14),T=a(T,b,e,6),E=a(E,x,e,6)}function P(e){k||!O||(c.useProgram(l.program),o.bindEngineGlobals(l,e),u!==null&&c.uniform2f(u,S,C),d!==null&&c.uniform1f(d,w),f!==null&&c.uniform1f(f,T),p!==null&&c.uniform1f(p,E*D.alphaCap),c.bindVertexArray(o.fullscreenVao),c.drawArrays(c.TRIANGLES,0,3))}function F(){k||(k=!0,o.disposeProgram(l))}return{setParameters:A,setQuality:j,resize:M,simulate:N,render:P,dispose:F}}function s(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-press-elastic-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{s as applyDomFallback,o as createEffect,n as manifest};