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
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;
uniform float uNodes;
uniform float uLinkWidth;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Force-directed link glow. Node positions are procedural rather than uploaded so
// this stays a one-draw ambient layer; the authoritative graph is DOM/SVG above it
// and remains keyboard-navigable (R2).
const int NODES = 10;

vec2 nodeAt(int i, float t) {
  float fi = float(i);
  float a = fi * 2.39996 + t * 0.12;
  float r = 0.18 + fract(fi * 91.7 * 0.137) * 0.26;
  return vec2(0.5, 0.5) + vec2(cos(a), sin(a) * 0.72) * r;
}

float segment(vec2 p, vec2 a, vec2 b) {
  vec2 pa = p - a;
  vec2 ba = b - a;
  float h = clamp(dot(pa, ba) / max(dot(ba, ba), 0.0001), 0.0, 1.0);
  return length(pa - ba * h);
}

void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  vec2 p = vUv * aspect;
  float glow = 0.0;
  for (int i = 0; i < NODES; i++) {
    vec2 a = nodeAt(i, uTime) * aspect;
    vec2 b = nodeAt((i + 3) % NODES, uTime) * aspect;
    float d = segment(p, a, b);
    glow += 1.0 - smoothstep(0.0, uLinkWidth, d);
  }
  float shaped = clamp(glow * (uNodes / float(NODES)), 0.0, 1.0);
  float alpha = shaped * uIntensity;
  fragColor = vec4(mix(uAtmoFar, uAtmoGlow, shaped) * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`force-graph-links`,title:`Force Graph Links`,composition:Object.freeze({slot:`ambient`,blend:`additive`,zIndex:19,priority:19,exclusiveGroup:`dataviz-graph`}),uniforms:Object.freeze({uNodes:Object.freeze({type:`float`,default:10,min:2,max:10,step:1,description:`Active node count (scales link brightness).`}),uLinkWidth:Object.freeze({type:`float`,default:.006,min:.002,max:.03,step:.001,description:`Link glow width.`}),uIntensity:Object.freeze({type:`float`,default:.32,min:0,max:.8,step:.01,description:`Link opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.7,alphaCap:.8}),low:Object.freeze({renderScale:.5,alphaCap:.5})}),cost:Object.freeze({budgetUnits:Object.freeze({high:1.45,medium:.95,low:.5}),gpuMilliseconds:Object.freeze({high:.52,medium:.33,low:.18}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uNodes`),u=s.getUniformLocation(c.program,`uLinkWidth`),d=s.getUniformLocation(c.program,`uIntensity`),f=n.uniforms.uNodes,p=n.uniforms.uLinkWidth,m=n.uniforms.uIntensity,h=r(o.uNodes,f),g=r(o.uLinkWidth,p),_=r(o.uIntensity,m),v=h,y=g,b=_,x=n.quality.medium,S=!0,C=!1;function w(e={}){Object.prototype.hasOwnProperty.call(e,`uNodes`)&&(h=r(e.uNodes,f)),Object.prototype.hasOwnProperty.call(e,`uLinkWidth`)&&(g=r(e.uLinkWidth,p)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,m))}function T(e,t){x=n.quality[e]||n.quality.medium,S=!!t}function E(){}function D(e){v=i(v,h,e,6),y=i(y,g,e,6),b=i(b,_,e,6)}function O(e){C||!S||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,v),u!==null&&s.uniform1f(u,y),d!==null&&s.uniform1f(d,b*x.alphaCap),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function k(){C||(C=!0,a.disposeProgram(c))}return{setParameters:w,setQuality:T,resize:E,simulate:D,render:O,dispose:k}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-force-graph-links-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};