import{n as e}from"./aurora-engine-contracts-CjO_kDw4.js";var t=`#version 300 es
out vec2 vUv;

void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,n=`#version 300 es
precision mediump float;

in vec2 vUv;

// Injected by Aurora. uTime advances only with the fixed-step clock, so it
// pauses, slows, seeks, and replays with the engine instead of wall time.
uniform float uTime;
uniform float uDeltaTime;
uniform vec2 uResolution;
uniform float uPixelRatio;
uniform float uQuality;
uniform vec3 uAtmoVoid;
uniform vec3 uAtmoNear;
uniform vec3 uAtmoFar;
uniform vec3 uAtmoGlow;

// Declared by this effect's manifest.uniforms table.
uniform float uIntensity;
uniform float uSpeed;
uniform float uAmplitude;

out vec4 fragColor;

void main() {
  vec2 p = vUv - 0.5;
  p.x *= uResolution.x / max(uResolution.y, 1.0);
  float radius = length(p);
  float phase = radius * (11.0 + uQuality * 8.0) - uTime * uSpeed;
  float crest = 0.5 + 0.5 * sin(phase);
  float ring = smoothstep(0.84, 0.22, abs(radius - (0.22 + crest * uAmplitude)));
  float edge = smoothstep(0.72, 0.12, radius);
  float alpha = ring * edge * uIntensity;
  fragColor = vec4(uAtmoGlow, alpha);
}
`,r=Object.freeze({schemaVersion:1,id:`aurora-pulse`,title:`Aurora Pulse`,composition:Object.freeze({slot:`ambient`,blend:`alpha`,zIndex:20,priority:20,exclusiveGroup:`ambient-pulse`}),uniforms:Object.freeze({uIntensity:Object.freeze({type:`float`,default:.18,min:0,max:.5}),uSpeed:Object.freeze({type:`float`,default:.7,min:0,max:3}),uAmplitude:Object.freeze({type:`float`,default:.13,min:.02,max:.3})}),quality:Object.freeze({high:Object.freeze({ringDensity:1,alphaCap:1}),medium:Object.freeze({ringDensity:.72,alphaCap:.82}),low:Object.freeze({ringDensity:.45,alphaCap:.56})}),cost:Object.freeze({budgetUnits:Object.freeze({high:2,medium:1.4,low:.8}),gpuMilliseconds:Object.freeze({high:.35,medium:.22,low:.12}),fill:`fullscreen`,allocation:`steady-state-zero-js`}),threading:Object.freeze({instructionGeneration:`worker-safe`,render:`main-or-offscreen`})});function i(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function a(a,o={}){let s=a.gl,c=a.createProgram({vertexSource:t,fragmentSource:n,label:r.id}),l=s.getUniformLocation(c.program,`uIntensity`),u=s.getUniformLocation(c.program,`uSpeed`),d=s.getUniformLocation(c.program,`uAmplitude`),f=r.uniforms.uIntensity,p=r.uniforms.uSpeed,m=r.uniforms.uAmplitude,h=i(o.uIntensity,f),g=h,_=i(o.uSpeed,p),v=i(o.uAmplitude,m),y=r.quality.medium,b=!0,x=!1;function S(e={}){Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(h=i(e.uIntensity,f)),Object.prototype.hasOwnProperty.call(e,`uSpeed`)&&(_=i(e.uSpeed,p)),Object.prototype.hasOwnProperty.call(e,`uAmplitude`)&&(v=i(e.uAmplitude,m))}function C(e,t){y=r.quality[e]||r.quality.medium,b=!!t}function w(e){let t=1-1e-4**Math.max(0,e);g+=(h-g)*t}function T(){}function E(t){x||!b||(s.useProgram(c.program),a.bindEngineGlobals(c,t),l!==null&&s.uniform1f(l,e(g)*y.alphaCap),u!==null&&s.uniform1f(u,_),d!==null&&s.uniform1f(d,v*y.ringDensity),s.bindVertexArray(a.fullscreenVao),s.drawArrays(s.TRIANGLES,0,3))}function D(){x||(x=!0,a.disposeProgram(c))}return{setParameters:S,setQuality:C,resize:T,simulate:w,render:E,dispose:D}}function o(e){return!e||typeof e.setAttribute!=`function`?()=>{}:(e.setAttribute(`data-aurora-pulse-fallback`,`true`),()=>e.removeAttribute(`data-aurora-pulse-fallback`))}export{o as applyDomFallback,a as createEffect,r as manifest};