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
uniform vec3 uAtmoNear;
uniform vec2 uTilt;
uniform float uDepth;
uniform float uIntensity;

in vec2 vUv;
out vec4 fragColor;

// Parallax sheen for a tilted card. The card itself is DOM and is transformed by
// CSS (D2); this layer only paints the moving specular band so that the sheen and
// the transform agree without WebGL owning any layout.
void main() {
  vec2 centred = vUv - 0.5;
  float along = dot(centred, normalize(uTilt + vec2(0.0001)));
  float band = exp(-pow(along * 3.2 - length(uTilt) * uDepth, 2.0) * 6.0);
  float edge = 1.0 - smoothstep(0.35, 0.5, length(centred * vec2(uResolution.x / max(uResolution.y, 1.0), 1.0)));
  float alpha = band * edge * uIntensity;
  fragColor = vec4(mix(uAtmoNear, uAtmoGlow, band) * alpha, alpha);
}
`,n=Object.freeze({schemaVersion:1,id:`hover-parallax-tilt`,title:`Hover Parallax Tilt`,composition:Object.freeze({slot:`interaction`,blend:`additive`,zIndex:39,priority:39,exclusiveGroup:`pointer-affordance`}),uniforms:Object.freeze({uTilt:Object.freeze({type:`vec2`,default:[0,0],min:-1,max:1,step:.001,description:`Normalised tilt vector from pointer offset.`}),uDepth:Object.freeze({type:`float`,default:.6,min:0,max:1.5,step:.01,description:`Parallax depth of the specular band.`}),uIntensity:Object.freeze({type:`float`,default:.3,min:0,max:.8,step:.01,description:`Sheen opacity.`})}),quality:Object.freeze({high:Object.freeze({renderScale:1,alphaCap:1}),medium:Object.freeze({renderScale:.75,alphaCap:.82}),low:Object.freeze({renderScale:.5,alphaCap:.55})}),cost:Object.freeze({budgetUnits:Object.freeze({high:.28,medium:.19,low:.11}),gpuMilliseconds:Object.freeze({high:.1,medium:.07,low:.04}),fill:`element`,allocation:`steady-state-zero-js`,estimatedDrawCalls:1,textureBytes:0}),threading:Object.freeze({instructionGeneration:`main-only`,render:`main-or-offscreen`})});function r(e,t){let n=Number(e);return Number.isFinite(n)?Math.min(t.max,Math.max(t.min,n)):t.default}function i(e,t){if(!Array.isArray(e)||e.length<2)return t.default;let n=Number(e[0]),r=Number(e[1]);return!Number.isFinite(n)||!Number.isFinite(r)?t.default:[Math.min(t.max,Math.max(t.min,n)),Math.min(t.max,Math.max(t.min,r))]}function a(e,t,n,r){let i=1-.001**(Math.max(0,Math.min(n,.25))*r);return e+(t-e)*i}function o(o,s={}){let c=o.gl,l=o.createProgram({vertexSource:e,fragmentSource:t,label:n.id}),u=c.getUniformLocation(l.program,`uTilt`),d=c.getUniformLocation(l.program,`uDepth`),f=c.getUniformLocation(l.program,`uIntensity`),p=n.uniforms.uTilt,m=n.uniforms.uDepth,h=n.uniforms.uIntensity,g=i(s.uTilt,p),_=r(s.uDepth,m),v=r(s.uIntensity,h),y=g[0],b=g[1],x=_,S=v,C=n.quality.medium,w=!0,T=!1;function E(e={}){Object.prototype.hasOwnProperty.call(e,`uTilt`)&&(g=i(e.uTilt,p)),Object.prototype.hasOwnProperty.call(e,`uDepth`)&&(_=r(e.uDepth,m)),Object.prototype.hasOwnProperty.call(e,`uIntensity`)&&(v=r(e.uIntensity,h))}function D(e,t){C=n.quality[e]||n.quality.medium,w=!!t}function O(){}function k(e){y=a(y,g[0],e,16),b=a(b,g[1],e,16),x=a(x,_,e,6),S=a(S,v,e,6)}function A(e){T||!w||(c.useProgram(l.program),o.bindEngineGlobals(l,e),u!==null&&c.uniform2f(u,y,b),d!==null&&c.uniform1f(d,x),f!==null&&c.uniform1f(f,S*C.alphaCap),c.bindVertexArray(o.fullscreenVao),c.drawArrays(c.TRIANGLES,0,3))}function j(){T||(T=!0,o.disposeProgram(l))}return{setParameters:E,setQuality:D,resize:O,simulate:k,render:A,dispose:j}}function s(e){if(!e||typeof e.setAttribute!=`function`)return()=>{};let t=`data-hover-parallax-tilt-fallback`,n=e.getAttribute(t);e.setAttribute(t,`true`);let r=!1;return()=>{r||(r=!0,n===null?e.removeAttribute(t):e.setAttribute(t,n))}}export{s as applyDomFallback,o as createEffect,n as manifest};