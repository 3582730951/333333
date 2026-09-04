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
uniform vec3 uAtmoGlow;
uniform float uRate;
uniform float uWidth;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Live-data heartbeat: a pulse travelling left to right whenever new samples land.
// uRate is fed from the real arrival rate, so a quiet stream visibly goes quiet
// instead of animating a lie.
void main() {
  float phase = fract(uTime * uRate);
  float band = exp(-pow((vUv.x - phase) / max(uWidth, 0.0001), 2.0) * 4.0);
  float centre = 1.0 - smoothstep(0.0, 0.5, abs(vUv.y - 0.5));
  float alpha = band * centre * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`data-pulse`,title:`Data Pulse`,composition:Object.freeze({slot:`ambient`,blend:`additive`,zIndex:26,priority:26,exclusiveGroup:`dataviz-live`}),uniforms:Object.freeze({uRate:Object.freeze({type:`float`,default:.5,min:0,max:3,step:.01,description:`Pulses per second, fed from the real sample arrival rate.`}),uWidth:Object.freeze({type:`float`,default:.14,min:.03,max:.4,step:.01,description:`Pulse width.`}),uIntensity:Object.freeze({type:`float`,default:.36,min:0,max:.9,step:.01,description:`Pulse opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,alphaCap:.82}),low:Object.freeze({renderScale:.5,alphaCap:.55})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.22,medium:.15,low:.09}),gpuMilliseconds:Object.freeze({high:.08,medium:.05,low:.03}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uRate`),u=s.getUniformLocation(c.program,`uWidth`),d=s.getUniformLocation(c.program,`uIntensity`),f=n.uniforms.uRate,p=n.uniforms.uWidth,m=n.uniforms.uIntensity,h=r(o.uRate,f),g=r(o.uWidth,p),_=r(o.uIntensity,m),v=h,y=g,b=_,x=n.quality.medium,S=!0,C=!1;function w(e={}){Object.prototype.hasOwnProperty.call(e,`uRate`)&&(h=r(e.uRate,f)),Object.prototype.hasOwnProperty.call(e,`uWidth`)&&(g=r(e.uWidth,p)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,m))}function T(e,t){x=n.quality[e]||n.quality.medium,S=!!t}function E(){}function D(e){v=i(v,h,e,6),y=i(y,g,e,6),b=i(b,_,e,6)}function O(e){C||!S||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,v),u!==null&&s.uniform1f(u,y),d!==null&&s.uniform1f(d,b*x.alphaCap),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function k(){C||(C=!0,a.disposeProgram(c))}return{setParameters:w,setQuality:T,resize:E,simulate:D,render:O,dispose:k}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-data-pulse-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};