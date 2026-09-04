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
uniform float uScale;
out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(123.34, 456.21));
  p += dot(p, p + 45.32);
  return fract(p.x * p.y);
}

float valueNoise(vec2 p) {
  vec2 cell = floor(p);
  vec2 local = fract(p);
  vec2 curve = local * local * (3.0 - 2.0 * local);
  float a = hash21(cell);
  float b = hash21(cell + vec2(1.0, 0.0));
  float c = hash21(cell + vec2(0.0, 1.0));
  float d = hash21(cell + vec2(1.0, 1.0));
  return mix(mix(a, b, curve.x), mix(c, d, curve.x), curve.y);
}

float field(vec2 p) {
  float value = 0.0;
  float weight = 0.58;
  for (int octave = 0; octave < 3; octave += 1) {
    value += valueNoise(p) * weight;
    p = mat2(1.72, -1.19, 1.19, 1.72) * p + vec2(3.1, -2.4);
    weight *= 0.52;
  }
  return value;
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  vec2 domain = p * (1.55 + uScale * 1.8);
  float first = field(domain + vec2(time * 0.13, -time * 0.09));
  float second = field(domain.yx * 1.42 + vec2(-time * 0.08, time * 0.11));
  float flow = fract(first + second * 0.72 + length(p) * 0.18);
  float filaments = smoothstep(0.58, 0.91, sin(flow * 18.8496) * 0.5 + 0.5);
  float vignette = 1.0 - smoothstep(0.28, 0.94, length(p));
  float qualityFade = mix(0.62, 1.0, clamp(uQuality, 0.0, 1.0));
  vec3 body = mix(uAtmoVoid, uAtmoNear, first * 0.9);
  vec3 hue = mix(uAtmoFar, uAtmoGlow, second * 0.78);
  vec3 color = mix(body, hue, filaments * 0.72);
  float alpha = (0.16 + filaments * 0.66) * vignette * qualityFade * uIntensity;
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`flowfield-noise`,title:`Flowfield Noise`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:12,priority:12,exclusiveGroup:`background-ambient`}),uniforms:Object.freeze({uIntensity:Object.freeze({type:`float`,default:.22,min:0,max:.45,step:.01,description:`Overall field opacity.`}),uSpeed:Object.freeze({type:`float`,default:.32,min:0,max:1.5,step:.01,description:`Fixed-clock field travel speed.`}),uScale:Object.freeze({type:`float`,default:1.05,min:.45,max:2.4,step:.01,description:`Noise-cell scale and filament density.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,detailScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,detailScale:.78,alphaCap:.8}),low:Object.freeze({renderScale:.5,detailScale:.56,alphaCap:.58})}),cost:Object.freeze({budgetUnits:Object.freeze({high:3,medium:2,low:.9}),gpuMilliseconds:Object.freeze({high:1.35,medium:.82,low:.42}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n){let r=1-.001**Math.max(0,Math.min(n,.25));return e+(t-e)*r}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=s.getUniformLocation(c.program,`uIntensity`),u=s.getUniformLocation(c.program,`uSpeed`),d=s.getUniformLocation(c.program,`uScale`),f=n.uniforms.uIntensity,p=n.uniforms.uSpeed,m=n.uniforms.uScale,h=r(o.uIntensity,f),g=r(o.uSpeed,p),_=r(o.uScale,m),v=h,y=g,b=_,x=n.quality.medium,S=!0,C=!1;function w(e={}){Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(h=r(e.uIntensity,f)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(g=r(e.uSpeed,p)),Object.prototype.hasOwnProperty.call(e,`uScale`)&&(_=r(e.uScale,m))}function T(e,t){x=n.quality[e]||n.quality.medium,S=!!t}function E(){}function D(e){v=i(v,h,e),y=i(y,g,e),b=i(b,_,e)}function O(e){C||!S||(s.useProgram(c.program),a.bindEngineGlobals(c,e),l!==null&&s.uniform1f(l,v*x.alphaCap),u!==null&&s.uniform1f(u,y),d!==null&&s.uniform1f(d,b*x.detailScale),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function k(){C||(C=!0,a.disposeProgram(c))}return{setParameters:w,setQuality:T,resize:E,simulate:D,render:O,dispose:k}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-aurora-flowfield-noise-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};