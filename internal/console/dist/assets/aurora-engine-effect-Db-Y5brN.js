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
uniform float uHeight;
uniform float uGrid;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Pseudo-3D data terrain via a raked grid: perspective comes from compressing the
// grid toward the horizon, not from a depth buffer, so it stays one fullscreen
// draw with zero geometry upload.
float ridge(vec2 p) {
  return sin(p.x * 3.1 + uTime * 0.18) * cos(p.y * 2.3 - uTime * 0.11);
}

void main() {
  float horizon = 0.62;
  float below = max(vUv.y - horizon, 0.0);
  float perspective = 1.0 / max(below * 6.0 + 0.12, 0.0001);
  vec2 p = vec2((vUv.x - 0.5) * perspective, perspective);
  float h = ridge(p) * uHeight;
  float lines = abs(fract(p.y * uGrid + h) - 0.5);
  float grid = 1.0 - smoothstep(0.0, 0.09, lines);
  float fade = 1.0 - smoothstep(horizon, 1.0, vUv.y);
  float mask = step(horizon, vUv.y);
  float alpha = grid * fade * mask * uIntensity;
  fragColor = vec4(mix(uAtmoNear, uAtmoGlow, grid) * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`terrain-3d`,title:`Terrain 3D`,composition:Object.freeze({slot:`base`,blend:`alpha`,zIndex:12,priority:12,exclusiveGroup:`dataviz-field`}),uniforms:Object.freeze({uHeight:Object.freeze({type:`float`,default:.35,min:0,max:1,step:.01,description:`Ridge height scale.`}),uGrid:Object.freeze({type:`float`,default:6,min:2,max:20,step:.5,description:`Grid line density along depth.`}),uIntensity:Object.freeze({type:`float`,default:.28,min:0,max:.8,step:.01,description:`Terrain opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.7,alphaCap:.78}),low:Object.freeze({renderScale:.5,alphaCap:.5})}),cost:Object.freeze({budgetUnits:Object.freeze({high:1.75,medium:1.15,low:.6}),gpuMilliseconds:Object.freeze({high:.63,medium:.4,low:.21}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uHeight`),u=s.getUniformLocation(c.program,`uGrid`),d=s.getUniformLocation(c.program,`uIntensity`),f=n.uniforms.uHeight,p=n.uniforms.uGrid,m=n.uniforms.uIntensity,h=r(o.uHeight,f),g=r(o.uGrid,p),_=r(o.uIntensity,m),v=h,y=g,b=_,x=n.quality.medium,S=!0,C=!1;function w(e={}){Object.prototype.hasOwnProperty.call(e,`uHeight`)&&(h=r(e.uHeight,f)),Object.prototype.hasOwnProperty.call(e,`uGrid`)&&(g=r(e.uGrid,p)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,m))}function T(e,t){x=n.quality[e]||n.quality.medium,S=!!t}function E(){}function D(e){v=i(v,h,e,6),y=i(y,g,e,6),b=i(b,_,e,6)}function O(e){C||!S||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,v),u!==null&&s.uniform1f(u,y),d!==null&&s.uniform1f(d,b*x.alphaCap),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function k(){C||(C=!0,a.disposeProgram(c))}return{setParameters:w,setQuality:T,resize:E,simulate:D,render:O,dispose:k}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-terrain-3d-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};