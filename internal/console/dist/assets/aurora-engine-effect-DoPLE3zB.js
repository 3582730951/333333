import"./aurora-engine-contracts-CjO_kDw4.js";var e=`#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,t=`#version 300 es
precision mediump float;

uniform vec2 uResolution;
uniform vec3 uAtmoGlow;
uniform vec2 uOrigin;
uniform float uProgress;
uniform float uSpread;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Success burst. The particle count is a compile-time constant so the loop is
// unrollable on ES 3.0 -- a uniform bound here would be a hard compile error, not
// a slow path.
const int PARTICLES = 12;

// Small-constant hash: keeps every literal inside the mediump range (|x| <= 16384)
// so the shader does not need highp. Same idiom as background/star-parallax.
float hash(float n) {
  float p = fract(n * 231.34);
  p += p * (p + 33.33);
  return fract(p * 512.77);
}

void main() {
  vec2 aspect = vec2(uResolution.x / max(uResolution.y, 1.0), 1.0);
  float total = 0.0;
  for (int i = 0; i < PARTICLES; i++) {
    float fi = float(i);
    float angle = hash(fi) * 6.28318;
    float speed = 0.45 + hash(fi + 7.0) * 0.55;
    vec2 offset = vec2(cos(angle), sin(angle)) * uProgress * speed * uSpread;
    float d = length((vUv - uOrigin - offset) * aspect);
    total += (1.0 - smoothstep(0.0, 0.022, d));
  }
  float fade = 1.0 - uProgress;
  float alpha = clamp(total, 0.0, 1.0) * fade * uIntensity;
  fragColor = vec4(uAtmoGlow * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`success-particles`,title:`Success Particles`,composition:Object.freeze({slot:`foreground`,blend:`additive`,zIndex:60,priority:60,exclusiveGroup:`feedback-burst`}),uniforms:Object.freeze({uOrigin:Object.freeze({type:`vec2`,default:[.5,.5],min:0,max:1,step:.001,description:`Burst origin in effect UV space.`}),uProgress:Object.freeze({type:`float`,default:0,min:0,max:1,step:.01,description:`Burst envelope owned by the host.`}),uSpread:Object.freeze({type:`float`,default:.5,min:.1,max:1.2,step:.01,description:`Particle travel distance.`}),uIntensity:Object.freeze({type:`float`,default:.6,min:0,max:1,step:.01,description:`Particle opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,alphaCap:.85}),low:Object.freeze({renderScale:.5,alphaCap:.6})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.52,medium:.34,low:.18}),gpuMilliseconds:Object.freeze({high:.19,medium:.12,low:.07}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`main-only`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t){if(!Array.isArray(e)||e.length<2)return t.default;let n=Number(e[0]),r=Number(e[1]);return!Number.isFinite(n)||!Number.isFinite(r)?t.default:[Math.min(t.max,Math.max(t.min,n)),Math.min(t.max,Math.max(t.min,r))]}function a(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function o(o,s={}){let c=o.gl,l=o.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),u=c.getUniformLocation(l.program,`uOrigin`),d=c.getUniformLocation(l.program,`uProgress`),f=c.getUniformLocation(l.program,`uSpread`),p=c.getUniformLocation(l.program,`uIntensity`),m=n.uniforms.uOrigin,h=n.uniforms.uProgress,g=n.uniforms.uSpread,_=n.uniforms.uIntensity,v=i(s.uOrigin,m),y=r(s.uProgress,h),b=r(s.uSpread,g),x=r(s.uIntensity,_),S=v[0],C=v[1],w=y,T=b,E=x,D=n.quality.medium,O=!0,k=!1;function A(e={}){Object.prototype.hasOwnProperty.call(e,`uOrigin`)&&(v=i(e.uOrigin,m)),Object.prototype.hasOwnProperty.call(e,`uProgress`)&&(y=r(e.uProgress,h)),Object.prototype.hasOwnProperty.call(e,`uSpread`)&&(b=r(e.uSpread,g)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(x=r(e.uIntensity,_))}function j(e,t){D=n.quality[e]||n.quality.medium,O=!!t}function M(){}function N(e){S=a(S,v[0],e,30),C=a(C,v[1],e,30),w=a(w,y,e,10),T=a(T,b,e,6),E=a(E,x,e,6)}function P(e){k||!O||(c.useProgram(l.program),o.bindEngineGlobals(l,e),u!==null&&c.uniform2f(u,S,C),d!==null&&c.uniform1f(d,w),f!==null&&c.uniform1f(f,T),p!==null&&c.uniform1f(p,E*D.alphaCap),c.bindVertexArray(o.fullscreenVao),c.drawArrays(c.TRIANGLES,0,3))}function F(){k||(k=!0,o.disposeProgram(l))}return{setParameters:A,setQuality:j,resize:M,simulate:N,render:P,dispose:F}}function s(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-success-particles-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{s as applyDomFallback,o as createEffect,n as manifest};