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
uniform float uDensity;
uniform float uSpeed;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Screen-space particle field. The count is a compile-time constant so the loop
// unrolls on ES 3.0; density scales brightness and cell size instead of the loop
// bound, because a uniform loop bound is a hard compile error here, not a slow path.
const int CELLS = 24;

// Small-constant hash: every literal stays inside mediump range (|x| <= 16384).
vec2 hash2(float n) {
  vec2 p = fract(vec2(n * 231.34, n * 512.77));
  p += dot(p, p + 33.33);
  return fract(p);
}

void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  float total = 0.0;
  for (int i = 0; i < CELLS; i++) {
    float fi = float(i);
    vec2 seed = hash2(fi + 1.0);
    float drift = uTime * uSpeed * (0.4 + seed.y * 0.8);
    vec2 pos = fract(seed + vec2(drift * 0.13, drift * 0.07));
    float d = length((vUv - pos) * aspect);
    total += (1.0 - smoothstep(0.0, 0.012 + uDensity * 0.02, d));
  }
  float alpha = clamp(total, 0.0, 1.0) * uIntensity;
  fragColor = vec4(mix(uAtmoFar, uAtmoGlow, clamp(total, 0.0, 1.0)) * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`gpu-particles`,title:`GPU Particles`,composition:Object.freeze({slot:`ambient`,blend:`additive`,zIndex:18,priority:18,exclusiveGroup:`dataviz-field`}),uniforms:Object.freeze({uDensity:Object.freeze({type:`float`,default:.4,min:0,max:1,step:.01,description:`Particle size and brightness scale.`}),uSpeed:Object.freeze({type:`float`,default:.35,min:0,max:1.5,step:.01,description:`Drift speed.`}),uIntensity:Object.freeze({type:`float`,default:.34,min:0,max:.9,step:.01,description:`Field opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.7,alphaCap:.8}),low:Object.freeze({renderScale:.5,alphaCap:.5})}),cost:Object.freeze({budgetUnits:Object.freeze({high:1.9,medium:1.2,low:.6}),gpuMilliseconds:Object.freeze({high:.68,medium:.42,low:.22}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uDensity`),u=s.getUniformLocation(c.program,`uSpeed`),d=s.getUniformLocation(c.program,`uIntensity`),f=n.uniforms.uDensity,p=n.uniforms.uSpeed,m=n.uniforms.uIntensity,h=r(o.uDensity,f),g=r(o.uSpeed,p),_=r(o.uIntensity,m),v=h,y=g,b=_,x=n.quality.medium,S=!0,C=!1;function w(e={}){Object.prototype.hasOwnProperty.call(e,`uDensity`)&&(h=r(e.uDensity,f)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(g=r(e.uSpeed,p)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,m))}function T(e,t){x=n.quality[e]||n.quality.medium,S=!!t}function E(){}function D(e){v=i(v,h,e,6),y=i(y,g,e,6),b=i(b,_,e,6)}function O(e){C||!S||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,v),u!==null&&s.uniform1f(u,y),d!==null&&s.uniform1f(d,b*x.alphaCap),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function k(){C||(C=!0,a.disposeProgram(c))}return{setParameters:w,setQuality:T,resize:E,simulate:D,render:O,dispose:k}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-gpu-particles-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};