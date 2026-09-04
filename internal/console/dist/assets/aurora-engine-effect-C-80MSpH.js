import"./aurora-engine-contracts-CjO_kDw4.js";var e=`#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,t=`#version 300 es
precision mediump float;

uniform vec3 uAtmoNear;
uniform vec3 uAtmoGlow;
uniform float uProgress;
uniform float uRows;
uniform float uStagger;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Staggered list entrance. Each row's local envelope is derived from its y band,
// so one draw covers the whole list instead of one effect instance per row --
// that is the difference between 1 draw call and 40.
void main() {
  float rows = max(uRows, 1.0);
  float row = floor(vUv.y * rows);
  float delay = row * uStagger / rows;
  float local = clamp((uProgress - delay) / max(1.0 - delay, 0.0001), 0.0, 1.0);
  float rise = 1.0 - local;
  float edge = 1.0 - smoothstep(0.0, 0.35, fract(vUv.y * rows));
  float alpha = rise * edge * uIntensity;
  fragColor = vec4(mix(uAtmoNear, uAtmoGlow, local) * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`list-stagger`,title:`List Stagger`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:24,priority:24,exclusiveGroup:`list-entrance`}),uniforms:Object.freeze({uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,step:.01,description:`Entrance envelope owned by the host.`}),uRows:Object.freeze({type:`float`,default:8,min:1,max:40,step:1,description:`Row count the list is showing.`}),uStagger:Object.freeze({type:`float`,default:.45,min:0,max:.9,step:.01,description:`Total stagger spread across rows.`}),uIntensity:Object.freeze({type:`float`,default:.3,min:0,max:.8,step:.01,description:`Entrance opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,alphaCap:.82}),low:Object.freeze({renderScale:.5,alphaCap:.55})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.25,medium:.17,low:.1}),gpuMilliseconds:Object.freeze({high:.09,medium:.06,low:.035}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uProgress`),u=s.getUniformLocation(c.program,`uRows`),d=s.getUniformLocation(c.program,`uStagger`),f=s.getUniformLocation(c.program,`uIntensity`),p=n.uniforms.uProgress,m=n.uniforms.uRows,h=n.uniforms.uStagger,g=n.uniforms.uIntensity,_=r(o.uProgress,p),v=r(o.uRows,m),y=r(o.uStagger,h),b=r(o.uIntensity,g),x=_,S=v,C=y,w=b,T=n.quality.medium,E=!0,D=!1;function O(e={}){Object.prototype.hasOwnProperty.call(e,`uProgress`)&&(_=r(e.uProgress,p)),Object.prototype.hasOwnProperty.call(e,`uRows`)&&(v=r(e.uRows,m)),Object.prototype.hasOwnProperty.call(e,`uStagger`)&&(y=r(e.uStagger,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(b=r(e.uIntensity,g))}function k(e,t){T=n.quality[e]||n.quality.medium,E=!!t}function A(){}function j(e){x=i(x,_,e,9),S=i(S,v,e,6),C=i(C,y,e,6),w=i(w,b,e,6)}function M(e){D||!E||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,x),u!==null&&s.uniform1f(u,S),d!==null&&s.uniform1f(d,C),f!==null&&s.uniform1f(f,w*T.alphaCap),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function N(){D||(D=!0,a.disposeProgram(c))}return{setParameters:O,setQuality:k,resize:A,simulate:j,render:M,dispose:N}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-list-stagger-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};