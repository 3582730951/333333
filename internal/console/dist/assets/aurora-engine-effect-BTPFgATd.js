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
uniform float uScale;
uniform float uSpeed;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Heat field over a data surface. Deliberately monotonic in luminance so it stays
// readable under both dichromacies -- hue alone must never carry the value, which
// is why the ramp mixes toward glow rather than rotating hue.
//
// Small-constant value noise: no literal exceeds the mediump range (|x| <= 16384),
// so this stays mediump on mobile GPUs. Same idiom as background/star-parallax.
float hash21(vec2 p) {
  p = fract(p * vec2(231.34, 512.77));
  p += dot(p, p + 33.33);
  return fract(p.x * p.y);
}

float noise(vec2 p) {
  vec2 i = floor(p);
  vec2 f = fract(p);
  vec2 u = f * f * (3.0 - 2.0 * f);
  float a = hash21(i);
  float b = hash21(i + vec2(1.0, 0.0));
  float c = hash21(i + vec2(0.0, 1.0));
  float d = hash21(i + vec2(1.0, 1.0));
  return mix(mix(a, b, u.x), mix(c, d, u.x), u.y);
}

void main() {
  vec2 p = vUv * uScale + vec2(uTime * uSpeed * 0.15, uTime * uSpeed * 0.09);
  float heat = noise(p) * 0.6 + noise(p * 2.1) * 0.4;
  float alpha = heat * uIntensity;
  fragColor = vec4(mix(uAtmoNear, uAtmoGlow, heat) * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`heat-flow`,title:`Heat Flow`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:16,priority:16,exclusiveGroup:`dataviz-field`}),uniforms:Object.freeze({uScale:Object.freeze({type:`float`,default:4,min:1,max:12,step:.1,description:`Noise field scale.`}),uSpeed:Object.freeze({type:`float`,default:.3,min:0,max:1.2,step:.01,description:`Advection speed.`}),uIntensity:Object.freeze({type:`float`,default:.3,min:0,max:.8,step:.01,description:`Heat opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.7,alphaCap:.8}),low:Object.freeze({renderScale:.5,alphaCap:.55})}),cost:Object.freeze({budgetUnits:Object.freeze({high:1.6,medium:1.05,low:.55}),gpuMilliseconds:Object.freeze({high:.58,medium:.36,low:.2}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uScale`),u=s.getUniformLocation(c.program,`uSpeed`),d=s.getUniformLocation(c.program,`uIntensity`),f=n.uniforms.uScale,p=n.uniforms.uSpeed,m=n.uniforms.uIntensity,h=r(o.uScale,f),g=r(o.uSpeed,p),_=r(o.uIntensity,m),v=h,y=g,b=_,x=n.quality.medium,S=!0,C=!1;function w(e={}){Object.prototype.hasOwnProperty.call(e,`uScale`)&&(h=r(e.uScale,f)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(g=r(e.uSpeed,p)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,m))}function T(e,t){x=n.quality[e]||n.quality.medium,S=!!t}function E(){}function D(e){v=i(v,h,e,6),y=i(y,g,e,6),b=i(b,_,e,6)}function O(e){C||!S||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,v),u!==null&&s.uniform1f(u,y),d!==null&&s.uniform1f(d,b*x.alphaCap),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function k(){C||(C=!0,a.disposeProgram(c))}return{setParameters:w,setQuality:T,resize:E,simulate:D,render:O,dispose:k}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-heat-flow-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};