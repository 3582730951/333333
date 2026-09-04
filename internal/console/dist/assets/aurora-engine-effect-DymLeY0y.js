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
uniform float uBreath;
out vec4 fragColor;

float orb(vec2 p, vec2 center, float radius) {
  float distanceToCenter = length(p - center);
  return 1.0 - smoothstep(radius * 0.52, radius, distanceToCenter);
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  float total = 0.0;
  vec3 color = uAtmoVoid;
  for (int index = 0; index < 4; index += 1) {
    float id = float(index);
    float phase = time * (0.62 + id * 0.13) + id * 2.1;
    vec2 center = vec2(
      sin(phase * 0.73 + id) * (0.22 + id * 0.045),
      cos(phase * 0.61 - id * 0.7) * (0.18 + id * 0.035)
    );
    float pulse = 1.0 + sin(time * (1.1 + id * 0.18) + id * 1.7) * uBreath * 0.18;
    float contribution = orb(p * (1.0 + id * 0.06) / uScale, center, (0.11 + id * 0.024) * pulse);
    float visibility = 1.0 - smoothstep(0.42, 1.0, id * (1.0 - uQuality));
    total += contribution * visibility;
    color += mix(uAtmoNear, uAtmoGlow, 0.3 + id * 0.16) * contribution * visibility;
  }
  float haze = smoothstep(0.98, 0.06, length(p)) * 0.08;
  color = mix(mix(uAtmoVoid, uAtmoFar, haze), color, clamp(total * 0.62, 0.0, 1.0));
  float alpha = min(0.78, (haze + total * 0.52) * uIntensity);
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`breathing-orbs`,title:`Breathing Orbs`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:18,priority:18,exclusiveGroup:`background-ambient`}),uniforms:Object.freeze({uIntensity:Object.freeze({type:`float`,default:.21,min:0,max:.48,step:.01,description:`Orb opacity.`}),uSpeed:Object.freeze({type:`float`,default:.3,min:0,max:1.3,step:.01,description:`Breathing cycle speed.`}),uScale:Object.freeze({type:`float`,default:.92,min:.45,max:1.7,step:.01,description:`Orb field scale.`}),uBreath:Object.freeze({type:`float`,default:.5,min:0,max:1,step:.01,description:`Pulse amplitude.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,orbCount:4,alphaCap:1}),medium:Object.freeze({renderScale:.75,orbCount:3,alphaCap:.8}),low:Object.freeze({renderScale:.5,orbCount:2,alphaCap:.58})}),cost:Object.freeze({budgetUnits:Object.freeze({high:2.1,medium:1.25,low:.54}),gpuMilliseconds:Object.freeze({high:.7,medium:.41,low:.22}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n){let r=1-.001**Math.max(0,Math.min(n,.25));return e+(t-e)*r}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=Object.keys(n.uniforms),u=Object.create(null),d=Object.create(null),f=Object.create(null);for(let e=0;e<l.length;e+=1){let t=l[e];u[t]=s.getUniformLocation(c.program,t),d[t]=r(o[t],n.uniforms[t]),f[t]=d[t]}let p=n.quality.medium,m=!0,h=!1;function g(e={}){for(let t=0;t<l.length;t+=1){let i=l[t];Object.prototype.hasOwnProperty.call(e,i)&&(d[i]=r(e[i],n.uniforms[i]))}}function _(e,t){p=n.quality[e]||n.quality.medium,m=!!t}function v(){}function y(e){for(let t=0;t<l.length;t+=1){let n=l[t];f[n]=i(f[n],d[n],e)}}function b(e){if(!(h||!m)){s.useProgram(c.program),a.bindEngineGlobals(c,e);for(let e=0;e<l.length;e+=1){let t=l[e],n=u[t];if(n===null)continue;let r=f[t];t===`uIntensity`&&(r*=p.alphaCap),t===`uScale`&&(r*=.82+p.alphaCap*.18),s.uniform1f(n,r)}s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3)}}function x(){h||(h=!0,a.disposeProgram(c))}return{setParameters:g,setQuality:_,resize:v,simulate:y,render:b,dispose:x}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-aurora-breathing-orbs-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};