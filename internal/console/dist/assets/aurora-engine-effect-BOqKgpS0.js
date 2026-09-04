import"./aurora-engine-contracts-CjO_kDw4.js";var e=`#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,t=`#version 300 es
precision mediump float;

uniform vec3 uAtmoGlow;
uniform float uRoll;
uniform float uBlur;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Digit-roll motion cue. D3 forbids WebGL owning text, so the digits stay DOM and
// this layer paints only the vertical motion blur band that sells the roll. The
// number itself is always readable and selectable even with WebGL off.
void main() {
  float travel = fract(uRoll);
  float band = exp(-pow((vUv.y - travel) / max(uBlur, 0.0001), 2.0) * 2.4);
  float settle = 1.0 - smoothstep(0.85, 1.0, travel);
  float alpha = band * settle * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`number-roll`,title:`Number Roll`,composition:Object.freeze({slot:`interaction`,blend:`additive`,zIndex:38,priority:38,exclusiveGroup:`numeric-feedback`}),uniforms:Object.freeze({uRoll:Object.freeze({type:`float`,default:0,min:0,max:1,step:.001,description:`Roll phase driven by the host value change.`}),uBlur:Object.freeze({type:`float`,default:.18,min:.04,max:.5,step:.01,description:`Vertical motion blur width.`}),uIntensity:Object.freeze({type:`float`,default:.28,min:0,max:.7,step:.01,description:`Blur band opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.7,alphaCap:.8}),low:Object.freeze({renderScale:.5,alphaCap:.5})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.18,medium:.12,low:.07}),gpuMilliseconds:Object.freeze({high:.07,medium:.045,low:.025}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uRoll`),u=s.getUniformLocation(c.program,`uBlur`),d=s.getUniformLocation(c.program,`uIntensity`),f=n.uniforms.uRoll,p=n.uniforms.uBlur,m=n.uniforms.uIntensity,h=r(o.uRoll,f),g=r(o.uBlur,p),_=r(o.uIntensity,m),v=h,y=g,b=_,x=n.quality.medium,S=!0,C=!1;function w(e={}){Object.prototype.hasOwnProperty.call(e,`uRoll`)&&(h=r(e.uRoll,f)),Object.prototype.hasOwnProperty.call(e,`uBlur`)&&(g=r(e.uBlur,p)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,m))}function T(e,t){x=n.quality[e]||n.quality.medium,S=!!t}function E(){}function D(e){v=i(v,h,e,12),y=i(y,g,e,6),b=i(b,_,e,6)}function O(e){C||!S||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,v),u!==null&&s.uniform1f(u,y),d!==null&&s.uniform1f(d,b*x.alphaCap),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function k(){C||(C=!0,a.disposeProgram(c))}return{setParameters:w,setQuality:T,resize:E,simulate:D,render:O,dispose:k}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-number-roll-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};