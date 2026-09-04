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
uniform vec3 uAtmoNear;
uniform vec2 uPointer;
uniform float uPull;
uniform float uRadius;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Magnetic pull: the field is strongest at the pointer and falls off smoothly.
// The button itself is DOM (D2) -- this layer only paints the attraction field
// beneath it, so text stays selectable and focusable.
float field(vec2 uv, vec2 centre, float radius) {
  float d = length((uv - centre) * vec2(uResolution.x / max(uResolution.y, 1.0), 1.0));
  return 1.0 - smoothstep(0.0, max(radius, 0.0001), d);
}

void main() {
  float pull = field(vUv, uPointer, uRadius);
  float shaped = pow(pull, 1.0 + uPull * 2.0);
  float breathe = 0.92 + 0.08 * sin(uTime * 2.1);
  vec3 tint = mix(uAtmoNear, uAtmoGlow, shaped);
  float alpha = shaped * uIntensity * breathe;
  fragColor = vec4(tint * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`magnetic-button`,title:`Magnetic Button`,composition:Object.freeze({slot:`interaction`,blend:`additive`,zIndex:40,priority:40,exclusiveGroup:`pointer-affordance`}),uniforms:Object.freeze({uPointer:Object.freeze({type:`vec2`,default:[.5,.5],min:0,max:1,step:.001,description:`Pointer position in effect UV space.`}),uPull:Object.freeze({type:`float`,default:.55,min:0,max:1,step:.01,description:`Falloff sharpness of the attraction field.`}),uRadius:Object.freeze({type:`float`,default:.28,min:.05,max:.8,step:.01,description:`Field radius in normalised units.`}),uIntensity:Object.freeze({type:`float`,default:.34,min:0,max:.8,step:.01,description:`Peak field opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1,followRate:1}),medium:Object.freeze({renderScale:.75,alphaCap:.85,followRate:.8}),low:Object.freeze({renderScale:.5,alphaCap:.6,followRate:.55})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.38,medium:.26,low:.15}),gpuMilliseconds:Object.freeze({high:.14,medium:.09,low:.05}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`main-only`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t){if(!Array.isArray(e)||e.length<2)return t.default;let n=Number(e[0]),r=Number(e[1]);return!Number.isFinite(n)||!Number.isFinite(r)?t.default:[Math.min(t.max,Math.max(t.min,n)),Math.min(t.max,Math.max(t.min,r))]}function a(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function o(o,s={}){let c=o.gl,l=o.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),u=c.getUniformLocation(l.program,`uPointer`),d=c.getUniformLocation(l.program,`uPull`),f=c.getUniformLocation(l.program,`uRadius`),p=c.getUniformLocation(l.program,`uIntensity`),m=n.uniforms.uPointer,h=n.uniforms.uPull,g=n.uniforms.uRadius,_=n.uniforms.uIntensity,v=i(s.uPointer,m),y=r(s.uPull,h),b=r(s.uRadius,g),x=r(s.uIntensity,_),S=v[0],C=v[1],w=y,T=b,E=x,D=n.quality.medium,O=!0,k=!1;function A(e={}){Object.prototype.hasOwnProperty.call(e,`uPointer`)&&(v=i(e.uPointer,m)),Object.prototype.hasOwnProperty.call(e,`uPull`)&&(y=r(e.uPull,h)),Object.prototype.hasOwnProperty.call(e,`uRadius`)&&(b=r(e.uRadius,g)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(x=r(e.uIntensity,_))}function j(e,t){D=n.quality[e]||n.quality.medium,O=!!t}function M(){}function N(e){S=a(S,v[0],e,D.followRate*18),C=a(C,v[1],e,D.followRate*18),w=a(w,y,e,6),T=a(T,b,e,6),E=a(E,x,e,6)}function P(e){k||!O||(c.useProgram(l.program),o.bindEngineGlobals(l,e),u!==null&&c.uniform2f(u,S,C),d!==null&&c.uniform1f(d,w),f!==null&&c.uniform1f(f,T),p!==null&&c.uniform1f(p,E*D.alphaCap),c.bindVertexArray(o.fullscreenVao),c.drawArrays(c.TRIANGLES,0,3))}function F(){k||(k=!0,o.disposeProgram(l))}return{setParameters:A,setQuality:j,resize:M,simulate:N,render:P,dispose:F}}function s(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-magnetic-button-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{s as applyDomFallback,o as createEffect,n as manifest};