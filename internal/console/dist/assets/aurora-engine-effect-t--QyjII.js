import"./aurora-engine-contracts-CjO_kDw4.js";var e=`#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,t=`#version 300 es
precision highp float;

in vec2 vUv;
uniform float uTime;
uniform float uDeltaTime;
uniform vec2 uResolution;
uniform float uPixelRatio;
uniform float uQuality;
uniform vec3 uAtmoVoid;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;
uniform float uIntensity;
uniform float uSpeed;
uniform float uSpread;
uniform float uDrift;
out vec4 fragColor;

float curtain(vec2 p, float lane, float time) {
  float frequency = 1.7 + lane * 0.52;
  float wobble = sin(p.x * frequency + time * (0.72 + lane * 0.19));
  wobble += sin(p.x * (frequency * 2.1) - time * 0.43 + lane) * 0.26;
  float center = 0.13 + lane * 0.22 + wobble * uDrift * 0.16;
  float distanceFromBand = abs(p.y - center);
  return 1.0 - smoothstep(0.02, 0.12 + uSpread * 0.22, distanceFromBand);
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  float light = 0.0;
  for (int lane = 0; lane < 3; lane += 1) {
    float laneValue = float(lane);
    float visibility = 1.0 - smoothstep(0.45, 1.0, laneValue * (1.0 - uQuality));
    light += curtain(p, laneValue, time) * visibility * (1.0 - laneValue * 0.18);
  }
  float edge = 1.0 - smoothstep(0.38, 0.98, length(p * vec2(0.72, 1.0)));
  float veil = smoothstep(-0.5, 0.42, p.y + sin(p.x * 1.3 + time * 0.25) * 0.08);
  vec3 backdrop = mix(uAtmoVoid, uAtmoFar, veil * 0.54);
  vec3 curtainColor = mix(uAtmoNear, uAtmoGlow, 0.36 + 0.38 * sin(time * 0.18 + p.x));
  vec3 color = mix(backdrop, curtainColor, clamp(light, 0.0, 1.0));
  float alpha = clamp(light * edge * uIntensity, 0.0, 0.72);
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`aurora-gradient`,title:`Aurora Gradient`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:13,priority:13,exclusiveGroup:`background-ambient`}),uniforms:Object.freeze({uIntensity:Object.freeze({type:`float`,default:.28,min:0,max:.5,step:.01,description:`Curtain opacity.`}),uSpeed:Object.freeze({type:`float`,default:.24,min:0,max:1.2,step:.01,description:`Fixed-clock curtain drift.`}),uSpread:Object.freeze({type:`float`,default:.68,min:.25,max:1.25,step:.01,description:`Vertical curtain spread.`}),uDrift:Object.freeze({type:`float`,default:.35,min:0,max:1,step:.01,description:`Horizontal wave displacement.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,bandCount:3,alphaCap:1}),medium:Object.freeze({renderScale:.75,bandCount:2,alphaCap:.8}),low:Object.freeze({renderScale:.5,bandCount:1,alphaCap:.58})}),cost:Object.freeze({budgetUnits:Object.freeze({high:2.5,medium:1.6,low:.75}),gpuMilliseconds:Object.freeze({high:.9,medium:.55,low:.3}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n){let r=1-.001**Math.max(0,Math.min(n,.25));return e+(t-e)*r}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uIntensity`),u=s.getUniformLocation(c.program,`uSpeed`),d=s.getUniformLocation(c.program,`uSpread`),f=s.getUniformLocation(c.program,`uDrift`),p=n.uniforms.uIntensity,m=n.uniforms.uSpeed,h=n.uniforms.uSpread,g=n.uniforms.uDrift,_=r(o.uIntensity,p),v=r(o.uSpeed,m),y=r(o.uSpread,h),b=r(o.uDrift,g),x=_,S=v,C=y,w=b,T=n.quality.medium,E=!0,D=!1;function O(e={}){Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(_=r(e.uIntensity,p)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(v=r(e.uSpeed,m)),Object.prototype.hasOwnProperty.call(e,`uSpread`)&&(y=r(e.uSpread,h)),Object.prototype.hasOwnProperty.call(e,`uDrift`)&&(b=r(e.uDrift,g))}function k(e,t){T=n.quality[e]||n.quality.medium,E=!!t}function A(){}function j(e){x=i(x,_,e),S=i(S,v,e),C=i(C,y,e),w=i(w,b,e)}function M(e){D||!E||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,x*T.alphaCap),u!==null&&s.uniform1f(u,S),d!==null&&s.uniform1f(d,C),f!==null&&s.uniform1f(f,w),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function N(){D||(D=!0,a.disposeProgram(c))}return{setParameters:O,setQuality:k,resize:A,simulate:j,render:M,dispose:N}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-aurora-gradient-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};