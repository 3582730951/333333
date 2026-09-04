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
uniform vec3 uAtmoNear;
uniform vec3 uAtmoGlow;
uniform float uSpeed;
uniform float uWidth;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Loading shimmer. It must be able to stop: prefers-reduced-motion is handled by
// the host dropping this effect entirely, and the DOM skeleton underneath stays
// visible on its own, so the loading state never depends on WebGL (R1).
void main() {
  float sweep = fract(uTime * uSpeed);
  float pos = sweep * 1.6 - 0.3;
  float band = exp(-pow((vUv.x + vUv.y * 0.35 - pos) / max(uWidth, 0.0001), 2.0));
  float alpha = band * uIntensity;
  fragColor = vec4(mix(uAtmoNear, uAtmoGlow, band) * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`skeleton-shimmer`,title:`Skeleton Shimmer`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:20,priority:20,exclusiveGroup:`loading-state`}),uniforms:Object.freeze({uSpeed:Object.freeze({type:`float`,default:.55,min:.1,max:2,step:.01,description:`Sweep cycles per second.`}),uWidth:Object.freeze({type:`float`,default:.22,min:.05,max:.6,step:.01,description:`Sweep band width.`}),uIntensity:Object.freeze({type:`float`,default:.26,min:0,max:.7,step:.01,description:`Shimmer opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.7,alphaCap:.8}),low:Object.freeze({renderScale:.5,alphaCap:.5})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.2,medium:.13,low:.08}),gpuMilliseconds:Object.freeze({high:.08,medium:.05,low:.03}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uSpeed`),u=s.getUniformLocation(c.program,`uWidth`),d=s.getUniformLocation(c.program,`uIntensity`),f=n.uniforms.uSpeed,p=n.uniforms.uWidth,m=n.uniforms.uIntensity,h=r(o.uSpeed,f),g=r(o.uWidth,p),_=r(o.uIntensity,m),v=h,y=g,b=_,x=n.quality.medium,S=!0,C=!1;function w(e={}){Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(h=r(e.uSpeed,f)),Object.prototype.hasOwnProperty.call(e,`uWidth`)&&(g=r(e.uWidth,p)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,m))}function T(e,t){x=n.quality[e]||n.quality.medium,S=!!t}function E(){}function D(e){v=i(v,h,e,6),y=i(y,g,e,6),b=i(b,_,e,6)}function O(e){C||!S||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,v),u!==null&&s.uniform1f(u,y),d!==null&&s.uniform1f(d,b*x.alphaCap),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function k(){C||(C=!0,a.disposeProgram(c))}return{setParameters:w,setQuality:T,resize:E,simulate:D,render:O,dispose:k}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-skeleton-shimmer-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};