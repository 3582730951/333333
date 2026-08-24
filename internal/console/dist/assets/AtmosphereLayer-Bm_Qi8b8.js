import{u as e}from"./browserStorage-BOQnGVsI.js";import{n as t,t as n}from"./jsx-runtime-B2iFcflP.js";import{a as r}from"./browserDocument-D4IUFITR.js";import{Tt as i,_t as a,bt as o,vt as s,wt as c}from"./RequestIDLine-NFFAokli.js";import{a as l}from"./AppErrorBoundary-Bihc1Nee.js";var u=e(t(),1),d=4e3,f=.002,p=`#version 300 es
// Fullscreen triangle, no vertex buffer: gl_VertexID drives the three corners.
// Cheaper than a quad (no diagonal seam, three vertices instead of six) and it
// needs no attribute state to be bound or torn down.
out vec2 vUv;
void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,m=`#version 300 es
precision mediump float;

// Must mirror the vertex stage's out-declaration exactly. GLSL ES 3.00 has no
// implicit varyings: omitting this line compiles the vertex shader fine and fails
// the fragment shader on first use, which reads as "WebGL unavailable" rather than
// as a typo. tests/atmosphere-shader.test.ts pins the two declarations together.
in vec2 vUv;

uniform vec2 uResolution;
uniform float uTime;
uniform vec2 uPointer;
uniform float uEnergy;
uniform float uAlpha;
uniform float uGrain;
uniform vec3 uVoid;
uniform vec3 uNear;
uniform vec3 uFar;
uniform vec3 uGlow;

out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(123.34, 456.21));
  p += dot(p, p + 45.32);
  return fract(p.x * p.y);
}

float vnoise(vec2 p) {
  vec2 i = floor(p);
  vec2 f = fract(p);
  vec2 u = f * f * (3.0 - 2.0 * f);
  float a = hash21(i);
  float b = hash21(i + vec2(1.0, 0.0));
  float c = hash21(i + vec2(0.0, 1.0));
  float d = hash21(i + vec2(1.0, 1.0));
  return mix(mix(a, b, u.x), mix(c, d, u.x), u.y);
}

// Four octaves is where the silhouette stops changing at this scale; a fifth costs
// a full-screen texture fetch pass worth of ALU and is invisible under the blur.
float fbm(vec2 p) {
  float value = 0.0;
  float amplitude = 0.5;
  for (int i = 0; i < 4; i++) {
    value += amplitude * vnoise(p);
    p *= 2.03;
    amplitude *= 0.5;
  }
  return value;
}

// A soft light well. Falloff is exp rather than smoothstep so neighbouring wells
// blend into one continuous field instead of showing their own edges -- that edge
// is what makes a CSS radial-gradient stack read as flat discs.
float well(vec2 uv, vec2 centre, float radius) {
  float d = length((uv - centre) / vec2(1.0, 0.72));
  return exp(-d * d / max(radius, 0.0001));
}

void main() {
  vec2 uv = vUv;
  float aspect = uResolution.x / max(uResolution.y, 1.0);
  vec2 p = vec2((uv.x - 0.5) * aspect, uv.y - 0.5);

  // Domain warp. The field is displaced by its own noise before it is sampled,
  // which is what gives the boundaries their organic, hand-mixed edge.
  float t = uTime * 0.026;
  vec2 warp = vec2(
    fbm(p * 1.6 + vec2(t, -t * 0.7)),
    fbm(p * 1.6 + vec2(4.7 - t * 0.8, 2.1 + t))
  );
  vec2 q = p + (warp - 0.5) * (0.42 + uEnergy * 0.22);

  // Three wells at three depths, evaluated once per colour channel at slightly
  // different offsets.
  //
  // That per-channel offset is chromatic aberration -- the RGB fringing a real lens
  // leaves toward the edge of frame, and the single cheapest thing that stops a
  // procedural gradient reading as "a CSS gradient with extra steps". The warp and
  // the noise are computed once and shared; only the well evaluations repeat, which
  // is a handful of exp() calls rather than another fbm.
  //
  // Strength scales with radius because that is where a lens actually disperses,
  // and it stays off in the middle third of the screen where the content sits.
  vec2 parallax = uPointer * 0.055;
  vec2 nearC = vec2(-0.34, 0.20) + parallax * 1.7 + vec2(sin(t * 1.7) * 0.05, cos(t * 1.3) * 0.04);
  vec2 farC  = vec2( 0.38, -0.16) + parallax * 0.9 + vec2(cos(t * 1.1) * 0.06, sin(t * 1.6) * 0.05);
  vec2 glowC = vec2( 0.02, -0.40) + parallax * 0.4 + vec2(sin(t * 0.9) * 0.08, 0.0);

  float radial = smoothstep(0.28, 1.05, length(p));
  vec2 disperse = normalize(p + vec2(1e-5)) * radial * 0.018;

  vec3 colour = uVoid;
  vec3 nearRGB = vec3(0.0);
  vec3 farRGB = vec3(0.0);
  vec3 glowRGB = vec3(0.0);
  for (int i = 0; i < 3; i++) {
    // -1, 0, +1 for R, G, B: green stays put and the outer channels split around it.
    vec2 offset = disperse * (float(i) - 1.0);
    vec2 qc = q + offset;
    nearRGB[i] = clamp(well(qc, nearC, 0.34), 0.0, 1.0);
    farRGB[i]  = clamp(well(qc, farC,  0.46), 0.0, 1.0);
    glowRGB[i] = clamp(well(qc, glowC, 0.30), 0.0, 1.0);
  }

  // Weights, not replacements. These were 0.85 / 0.80 / 0.10+0.42E, which drove the
  // wells almost all the way to their own colour and turned every dark page into a
  // saturated green wash -- the field became the subject instead of setting a mood
  // under one. The console is an instrument; the background's job is to give it
  // somewhere to sit.
  colour = mix(colour, uFar,  farRGB  * 0.50);
  colour = mix(colour, uNear, nearRGB * 0.44);

  // The accent is the only saturated thing on screen and it stays scarce: its
  // weight is driven by live pool energy, so the field brightens under load and
  // settles when the pool is idle. That is the whole "show, don't tell" of it.
  vec3 accent = glowRGB * (0.04 + uEnergy * 0.20);
  colour = mix(colour, uGlow, accent);

  // Depth ramp plus vignette. Both exist to protect legibility: content sits on
  // this layer, so the corners and the top must fall back toward the void.
  float depth = smoothstep(-0.55, 0.55, q.y * 0.6 + fbm(q * 0.9 + t) * 0.25);
  colour = mix(colour, uVoid, (1.0 - depth) * 0.38);
  // Tightened from smoothstep(1.25, 0.28, ...): the old falloff only pulled back at
  // the extreme corners, so the field reached every edge of every page. Now it
  // concentrates and most of the screen is within a few percent of the void.
  float vignette = smoothstep(1.02, 0.16, length(p * vec2(0.82, 1.05)));
  colour = mix(uVoid, colour, vignette);

  // A measured dot lattice, not a texture.
  //
  // Photographic grain is the wrong idiom for an instrument: it says "film", and
  // this surface is a control panel. A regular lattice of sub-pixel dots says
  // "measured" instead -- the same register as a blueprint or an oscilloscope
  // graticule -- and it is what the reference gallery uses under its loader. It
  // rides on top of the field at a fixed device-pixel pitch so it never scales
  // with the render buffer, and it fades out where the field is brightest so it
  // never sits on top of the accent.
  vec2 cell = fract(gl_FragCoord.xy / 6.0) - 0.5;
  float lattice = 1.0 - smoothstep(0.0, 0.34, length(cell));
  colour += lattice * uGrain * 0.55 * (1.0 - vignette * 0.35);

  // Animated noise underneath it, at a fraction of the amplitude. Its only job now
  // is to break the banding an 8-bit gradient this large would otherwise show; the
  // lattice above carries the texture.
  float grain = hash21(gl_FragCoord.xy + fract(uTime) * 91.7) - 0.5;
  colour += grain * uGrain * 0.5;

  fragColor = vec4(clamp(colour, 0.0, 1.0), uAlpha);
}
`;function h(e,t,n,r){let i=e.createShader(t);return i?(e.shaderSource(i,n),e.compileShader(i),e.getShaderParameter(i,e.COMPILE_STATUS)?i:(r(`shader compile failed: ${e.getShaderInfoLog(i)}`),e.deleteShader(i),null)):null}function g(e,t){let n=h(e,e.VERTEX_SHADER,p,t),r=h(e,e.FRAGMENT_SHADER,m,t);if(!n||!r)return n&&e.deleteShader(n),r&&e.deleteShader(r),null;let i=e.createProgram();return i?(e.attachShader(i,n),e.attachShader(i,r),e.linkProgram(i),e.detachShader(i,n),e.detachShader(i,r),e.deleteShader(n),e.deleteShader(r),e.getProgramParameter(i,e.LINK_STATUS)?i:(t(`program link failed: ${e.getProgramInfoLog(i)}`),e.deleteProgram(i),null)):(e.deleteShader(n),e.deleteShader(r),null)}function _(e){let t=String(e||``).trim();if(!t)return null;if(t.charCodeAt(0)===35){let e=t.slice(1),n=e.length===3?e.split(``).map(e=>e+e).join(``):e;return/^[0-9a-f]{6}$/i.test(n)?[0,2,4].map(e=>parseInt(n.slice(e,e+2),16)/255):null}let n=t.match(/-?\d*\.?\d+/g);return!n||n.length<3?null:n.slice(0,3).map(e=>Math.min(1,Math.max(0,Number(e)/255)))}var v=[`uResolution`,`uTime`,`uPointer`,`uEnergy`,`uAlpha`,`uGrain`,`uVoid`,`uNear`,`uFar`,`uGlow`];function y(e,{onDiagnostic:t=()=>{}}={}){if(!e||typeof e.getContext!=`function`||typeof WebGL2RenderingContext>`u`)return null;let n=null;try{n=e.getContext(`webgl2`,{alpha:!0,antialias:!1,depth:!1,stencil:!1,preserveDrawingBuffer:!1,powerPreference:`low-power`})}catch(e){t(`webgl2 context refused: ${e}`),n=null}if(!n)return null;let r=g(n,t);if(!r)return null;let a=n.createVertexArray(),s={};for(let e of v)s[e]=n.getUniformLocation(r,e);let c={width:0,height:0,scale:1,time:0,energy:0,targetEnergy:0,pointer:[0,0],targetPointer:[0,0],alpha:1,grain:.05,colours:{uVoid:[0,0,0],uNear:[0,0,0],uFar:[0,0,0],uGlow:[0,0,0]}},l=null,u=!1,p=!1,m=0,h=0,y=0,b=()=>{p||(h=y+d,u||D())},x=()=>Math.abs(c.targetEnergy-c.energy)<f&&Math.abs(c.targetPointer[0]-c.pointer[0])<f&&Math.abs(c.targetPointer[1]-c.pointer[1])<f,S=(t,r,i)=>{if(p)return;let a=Math.min(Math.max(i||1,1),2)*.62,o=Math.max(1,Math.round(t*a)),s=Math.max(1,Math.round(r*a));o===c.width&&s===c.height||(c.width=o,c.height=s,e.width=o,e.height=s,n.viewport(0,0,o,s),b())},C=e=>{for(let t of[`uVoid`,`uNear`,`uFar`,`uGlow`]){let n=_(e?.[t]);n&&(c.colours[t]=n)}Number.isFinite(e?.alpha)&&(c.alpha=Math.min(1,Math.max(0,e.alpha))),Number.isFinite(e?.grain)&&(c.grain=Math.min(.4,Math.max(0,e.grain))),b()},w=e=>{if(!Number.isFinite(e))return;let t=Math.min(1,Math.max(0,e));t!==c.targetEnergy&&(c.targetEnergy=t,b())},T=(e,t)=>{let n=[Math.min(1,Math.max(-1,Number(e)||0)),Math.min(1,Math.max(-1,Number(t)||0))];n[0]===c.targetPointer[0]&&n[1]===c.targetPointer[1]||(c.targetPointer=n,b())},E=e=>{if(p||!u)return;if(n.isContextLost()){u=!1,l=null;return}let t=m?Math.min(64,e-m):16;m=e,y+=t,c.time+=t/1e3;let o=1-.0016**(t/1e3);if(c.energy+=(c.targetEnergy-c.energy)*o,c.pointer[0]+=(c.targetPointer[0]-c.pointer[0])*o,c.pointer[1]+=(c.targetPointer[1]-c.pointer[1])*o,n.useProgram(r),n.bindVertexArray(a),n.uniform2f(s.uResolution,c.width,c.height),n.uniform1f(s.uTime,c.time),n.uniform2f(s.uPointer,c.pointer[0],c.pointer[1]),n.uniform1f(s.uEnergy,c.energy),n.uniform1f(s.uAlpha,c.alpha),n.uniform1f(s.uGrain,c.grain),n.uniform3fv(s.uVoid,c.colours.uVoid),n.uniform3fv(s.uNear,c.colours.uNear),n.uniform3fv(s.uFar,c.colours.uFar),n.uniform3fv(s.uGlow,c.colours.uGlow),n.drawArrays(n.TRIANGLES,0,3),y>h&&x()){u=!1,l=null;return}l=i(E)};function D(){p||u||n.isContextLost()||(u=!0,m=0,h<=y&&(h=y+d),l=i(E))}let O=()=>{u=!1,l!=null&&o(l),l=null};return{resize:S,setPalette:C,setEnergy:w,setPointer:T,start:D,stop:O,renderStill:()=>{p||(c.energy=c.targetEnergy,c.pointer=[...c.targetPointer],u=!0,E(0),O())},dispose:()=>{O(),p=!0;try{n.deleteVertexArray(a),n.deleteProgram(r)}catch{}}}}var b=n(),x=[`--pool-atmo-void`,`--pool-atmo-near`,`--pool-atmo-far`,`--pool-atmo-glow`,`--pool-atmo-alpha`,`--pool-grain-alpha`];function S(){let e=r([...x]);return{uVoid:e[`--pool-atmo-void`],uNear:e[`--pool-atmo-near`],uFar:e[`--pool-atmo-far`],uGlow:e[`--pool-atmo-glow`],alpha:Number(e[`--pool-atmo-alpha`])||1,grain:Number(e[`--pool-grain-alpha`])||0}}function C(){try{return!!window.matchMedia?.(`(prefers-reduced-motion: reduce)`).matches}catch{return!1}}function w({subscribe:e}){let t=(0,u.useRef)(null),[n,r]=(0,u.useState)(!1);return(0,u.useEffect)(()=>{let n=t.current;if(!n)return;let i=y(n,{onDiagnostic:e=>l(Error(e),{source:`atmosphere.webgl`,componentStack:`AtmosphereLayer`})});if(!i)return;let o=C(),u=!1;r(!0);let d=()=>{if(u)return;let e=n.getBoundingClientRect();i.resize(e.width,e.height,window.devicePixelRatio||1),o&&i.renderStill()};i.setPalette(S()),d();let f=new MutationObserver(()=>{u||(i.setPalette(S()),o&&i.renderStill())});f.observe(document.documentElement,{attributes:!0,attributeFilter:[`data-theme`]});let p=typeof ResizeObserver==`function`?new ResizeObserver(d):null;p?.observe(n);let m=p?()=>{}:s(`resize`,d),h=()=>{},g=()=>{},_=()=>{};return o?i.renderStill():(h=s(`pointermove`,e=>{let t=window.innerWidth||1,n=window.innerHeight||1;i.setPointer(e.clientX/t*2-1,e.clientY/n*2-1)},{passive:!0}),g=a(`visibilitychange`,()=>{u||(c()?i.start():i.stop())}),c()&&i.start()),e&&(_=e(e=>{u||(i.setEnergy(Number(e?.energy)||0),o&&i.renderStill())})),()=>{u=!0,r(!1),_(),h(),g(),m(),p?.disconnect(),f.disconnect(),i.dispose()}},[e]),(0,b.jsxs)(`div`,{className:`pool-atmosphere`,"data-webgl":n?`true`:`false`,"aria-hidden":`true`,children:[(0,b.jsx)(`canvas`,{ref:t,className:`pool-atmosphere__canvas`}),(0,b.jsx)(`div`,{className:`pool-atmosphere__grain`})]})}export{w as default};