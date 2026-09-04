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
uniform float uDensity;
uniform float uWave;
out vec4 fragColor;

float gridLine(float coordinate, float thickness) {
  float distanceToCenter = abs(fract(coordinate) - 0.5);
  return 1.0 - smoothstep(thickness, thickness + 0.024, distanceToCenter);
}

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float time = uTime * uSpeed;
  float waveA = sin(p.x * 5.4 + time * 1.6) * uWave * 0.09;
  float waveB = cos(p.y * 4.7 - time * 1.2) * uWave * 0.07;
  vec2 warped = p + vec2(waveB, waveA);
  vec2 cells = warped * (6.5 + uDensity * 8.0);
  float thickness = mix(0.012, 0.021, uQuality);
  float lines = max(gridLine(cells.x, thickness), gridLine(cells.y, thickness));
  float horizon = smoothstep(-0.5, 0.44, p.y + waveA * 1.8);
  float fade = 1.0 - smoothstep(0.42, 1.08, length(p));
  vec3 field = mix(uAtmoVoid, uAtmoFar, horizon * 0.32);
  vec3 lineColor = mix(uAtmoNear, uAtmoGlow, 0.54 + 0.28 * sin(time * 0.35 + p.x));
  vec3 color = mix(field, lineColor, lines);
  float alpha = (0.09 * horizon + lines * 0.78) * fade * uIntensity;
  fragColor = vec4(color, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`grid-wave`,title:`Grid Wave`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:16,priority:16,exclusiveGroup:`background-ambient`}),uniforms:Object.freeze({uIntensity:Object.freeze({type:`float`,default:.17,min:0,max:.42,step:.01,description:`Grid opacity.`}),uSpeed:Object.freeze({type:`float`,default:.34,min:0,max:1.4,step:.01,description:`Fixed-clock wave speed.`}),uDensity:Object.freeze({type:`float`,default:1,min:.35,max:2.2,step:.01,description:`Grid cell density.`}),uWave:Object.freeze({type:`float`,default:.38,min:0,max:1.2,step:.01,description:`Grid displacement amplitude.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,detailScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,detailScale:.78,alphaCap:.8}),low:Object.freeze({renderScale:.5,detailScale:.55,alphaCap:.6})}),cost:Object.freeze({budgetUnits:Object.freeze({high:1.8,medium:1.05,low:.46}),gpuMilliseconds:Object.freeze({high:.56,medium:.33,low:.18}),fill:`fullscreen`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t,n){let r=1-.001**Math.max(0,Math.min(n,.25));return e+(t-e)*r}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),l=Object.keys(n.uniforms),u=Object.create(null),d=Object.create(null),f=Object.create(null);for(let e=0;e<l.length;e+=1){let t=l[e];u[t]=s.getUniformLocation(c.program,t),d[t]=r(o[t],n.uniforms[t]),f[t]=d[t]}let p=n.quality.medium,m=!0,h=!1;function g(e={}){for(let t=0;t<l.length;t+=1){let i=l[t];Object.prototype.hasOwnProperty.call(e,i)&&(d[i]=r(e[i],n.uniforms[i]))}}function _(e,t){p=n.quality[e]||n.quality.medium,m=!!t}function v(){}function y(e){for(let t=0;t<l.length;t+=1){let n=l[t];f[n]=i(f[n],d[n],e)}}function b(e){if(!(h||!m)){s.useProgram(c.program),a.bindEngineGlobals(c,e);for(let e=0;e<l.length;e+=1){let t=l[e],n=u[t];if(n===null)continue;let r=f[t];t===`uIntensity`&&(r*=p.alphaCap),t===`uDensity`&&(r*=p.detailScale),s.uniform1f(n,r)}s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3)}}function x(){h||(h=!0,a.disposeProgram(c))}return{setParameters:g,setQuality:_,resize:v,simulate:y,render:b,dispose:x}}function o(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-aurora-grid-wave-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{o as applyDomFallback,a as createEffect,n as manifest};