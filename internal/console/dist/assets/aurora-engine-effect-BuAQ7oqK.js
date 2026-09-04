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
uniform float uProgress;
uniform float uThickness;
uniform float uAmplitude;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Trend-line draw-on. The real series is an SVG path in the DOM; this paints the
// travelling highlight only, so the chart is complete and readable the instant it
// renders even when the effect never loads.
void main() {
  float curve = 0.5 + sin(vUv.x * 9.0) * 0.13 * uAmplitude + sin(vUv.x * 3.1) * 0.07 * uAmplitude;
  float line = 1.0 - smoothstep(0.0, uThickness, abs(vUv.y - curve));
  float drawn = 1.0 - smoothstep(uProgress - 0.04, uProgress, vUv.x);
  float head = exp(-pow((vUv.x - uProgress) / 0.05, 2.0) * 2.0);
  float alpha = line * (drawn * 0.55 + head) * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`trend-stroke`,title:`Trend Stroke`,composition:Object.freeze({slot:`ambient`,blend:`additive`,zIndex:28,priority:28,exclusiveGroup:`dataviz-live`}),uniforms:Object.freeze({uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,step:.01,description:`Draw-on progress owned by the host.`}),uThickness:Object.freeze({type:`float`,default:.02,min:.004,max:.08,step:.002,description:`Highlight thickness.`}),uAmplitude:Object.freeze({type:`float`,default:1,min:0,max:2,step:.01,description:`Curve amplitude scale.`}),uIntensity:Object.freeze({type:`float`,default:.38,min:0,max:.9,step:.01,description:`Highlight opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,alphaCap:.82}),low:Object.freeze({renderScale:.5,alphaCap:.55})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.3,medium:.2,low:.11}),gpuMilliseconds:Object.freeze({high:.11,medium:.07,low:.04}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uProgress`),u=s.getUniformLocation(c.program,`uThickness`),d=s.getUniformLocation(c.program,`uAmplitude`),f=s.getUniformLocation(c.program,`uIntensity`),p=n.uniforms.uProgress,m=n.uniforms.uThickness,h=n.uniforms.uAmplitude,g=n.uniforms.uIntensity,_=r(o.uProgress,p),v=r(o.uThickness,m),y=r(o.uAmplitude,h),b=r(o.uIntensity,g),x=_,S=v,C=y,w=b,T=n.quality.medium,E=!0,D=!1;function O(e={}){Object.prototype.hasOwnProperty.call(e,`uProgress`)&&(_=r(e.uProgress,p)),Object.prototype.hasOwnProperty.call(e,`uThickness`)&&(v=r(e.uThickness,m)),Object.prototype.hasOwnProperty.call(e,`uAmplitude`)&&(y=r(e.uAmplitude,h)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(b=r(e.uIntensity,g))}function k(e,t){T=n.quality[e]||n.quality.medium,E=!!t}function A(){}function j(e){x=i(x,_,e,8),S=i(S,v,e,6),C=i(C,y,e,6),w=i(w,b,e,6)}function M(e){D||!E||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,x),u!==null&&s.uniform1f(u,S),d!==null&&s.uniform1f(d,C),f!==null&&s.uniform1f(f,w*T.alphaCap),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function N(){D||(D=!0,a.disposeProgram(c))}return{setParameters:O,setQuality:k,resize:A,simulate:j,render:M,dispose:N}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-trend-stroke-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};