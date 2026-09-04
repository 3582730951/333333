import"./aurora-engine-contracts-CjO_kDw4.js";var e=`#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,t=`#version 300 es
precision mediump float;

uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;
uniform float uVelocity;
uniform float uOverscroll;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Scroll inertia readout: a velocity smear plus an overscroll cushion at the
// edge being pushed against. The list itself scrolls natively in the DOM (D2) --
// this only paints the physical cue, so keyboard and screen-reader scrolling are
// untouched.
void main() {
  float dir = sign(uVelocity);
  float speed = clamp(abs(uVelocity), 0.0, 1.0);
  float edge = dir > 0.0 ? vUv.y : 1.0 - vUv.y;
  float smear = (1.0 - smoothstep(0.0, 0.45, edge)) * speed;
  float cushion = (1.0 - smoothstep(0.0, 0.22, edge)) * abs(uOverscroll);
  float alpha = (smear * 0.45 + cushion * 0.8) * uIntensity;
  fragColor = vec4(mix(uAtmoFar, uAtmoGlow, cushion) * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`inertial-scroll`,title:`Inertial Scroll`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:22,priority:22,exclusiveGroup:`scroll-feedback`}),uniforms:Object.freeze({uVelocity:Object.freeze({type:`float`,default:0,min:-1,max:1,step:.01,description:`Normalised scroll velocity.`}),uOverscroll:Object.freeze({type:`float`,default:0,min:-1,max:1,step:.01,description:`Overscroll displacement past the edge.`}),uIntensity:Object.freeze({type:`float`,default:.32,min:0,max:.8,step:.01,description:`Cue opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.7,alphaCap:.8}),low:Object.freeze({renderScale:.5,alphaCap:.55})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.22,medium:.15,low:.08}),gpuMilliseconds:Object.freeze({high:.08,medium:.05,low:.03}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`main-only`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uVelocity`),u=s.getUniformLocation(c.program,`uOverscroll`),d=s.getUniformLocation(c.program,`uIntensity`),f=n.uniforms.uVelocity,p=n.uniforms.uOverscroll,m=n.uniforms.uIntensity,h=r(o.uVelocity,f),g=r(o.uOverscroll,p),_=r(o.uIntensity,m),v=h,y=g,b=_,x=n.quality.medium,S=!0,C=!1;function w(e={}){Object.prototype.hasOwnProperty.call(e,`uVelocity`)&&(h=r(e.uVelocity,f)),Object.prototype.hasOwnProperty.call(e,`uOverscroll`)&&(g=r(e.uOverscroll,p)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,m))}function T(e,t){x=n.quality[e]||n.quality.medium,S=!!t}function E(){}function D(e){v=i(v,h,e,22),y=i(y,g,e,18),b=i(b,_,e,6)}function O(e){C||!S||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,v),u!==null&&s.uniform1f(u,y),d!==null&&s.uniform1f(d,b*x.alphaCap),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function k(){C||(C=!0,a.disposeProgram(c))}return{setParameters:w,setQuality:T,resize:E,simulate:D,render:O,dispose:k}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-inertial-scroll-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};