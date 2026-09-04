import{i as e,l as t}from"./browserLifecycle-DPKUXVzn.js";var n=4e3,r=.002,i=[`auto`,`high`,`balanced`,`low`,`still`,`off`];function a(e){let t=String(e||``).trim().toLowerCase();return i.includes(t)?t:`auto`}var o={high:{dprCap:1.5,renderScale:.78,activeFrameMs:16,idleFrameMs:33,shaderQuality:1},balanced:{dprCap:1.25,renderScale:.7,activeFrameMs:33,idleFrameMs:33,shaderQuality:.68},low:{dprCap:1,renderScale:.62,activeFrameMs:50,idleFrameMs:66,shaderQuality:.28},still:{dprCap:1,renderScale:.62,activeFrameMs:1/0,idleFrameMs:1/0,shaderQuality:.28}},s=`#version 300 es
// Fullscreen triangle, no vertex buffer: gl_VertexID drives the three corners.
// Cheaper than a quad (no diagonal seam, three vertices instead of six) and it
// needs no attribute state to be bound or torn down.
out vec2 vUv;
void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,c=`#version 300 es
precision mediump float;

// Must mirror the vertex stage's out-declaration exactly. GLSL ES 3.00 has no
// implicit varyings: omitting this line compiles the vertex shader fine and fails
// the fragment shader on first use, which reads as "WebGL unavailable" rather than
// as a typo. tests/atmosphere-shader.test.ts pins the two declarations together.
in vec2 vUv;

uniform vec2 uResolution;
uniform float uTime;
uniform vec2 uPointer;
uniform vec2 uFocus;
uniform float uEnergy;
uniform float uActivity;
uniform float uScroll;
uniform float uVelocity;
uniform float uQuality;
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
    if (i >= 2 && uQuality < 0.5) break;
    if (i >= 3 && uQuality < 0.82) break;
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
  parallax += vec2(0.0, uScroll * 0.026);
  vec2 nearC = vec2(-0.34, 0.20) + parallax * 1.7 + vec2(sin(t * 1.7) * 0.05, cos(t * 1.3) * 0.04);
  vec2 farC  = vec2( 0.38, -0.16) + parallax * 0.9 + vec2(cos(t * 1.1) * 0.06, sin(t * 1.6) * 0.05);
  vec2 glowC = mix(vec2(0.02, -0.40), uFocus, 0.22) + parallax * 0.4 + vec2(sin(t * 0.9) * 0.08, 0.0);

  float radial = smoothstep(0.28, 1.05, length(p));
  vec2 disperse = normalize(p + vec2(1e-5)) * radial * 0.018 * smoothstep(0.45, 0.9, uQuality);

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
  vec3 accent = glowRGB * (0.04 + uEnergy * 0.16 + uActivity * 0.10);
  colour = mix(colour, uGlow, accent);

  // Event-driven volumetric light and a restrained metallic sweep. Both reuse the
  // same single pass and palette uniforms: no framebuffer, texture or brand colour
  // is hidden in the shader. Velocity/activity fade to zero after interaction, so
  // an idle operations screen becomes genuinely still.
  float beamAxis = abs((p.x - uFocus.x * 0.28) + p.y * 0.24);
  float beam = exp(-beamAxis * (7.0 + 4.0 * (1.0 - uQuality)));
  beam *= smoothstep(-0.62, 0.54, p.y) * (0.015 + uVelocity * 0.055 + uActivity * 0.035);
  colour = mix(colour, uGlow, clamp(beam, 0.0, 0.12));

  float causticPhase = q.x * 8.0 + q.y * 5.0 + t * 5.0;
  float caustic = pow(max(0.0, sin(causticPhase) * 0.5 + 0.5), 8.0);
  caustic *= uQuality * (0.008 + uActivity * 0.025) * (1.0 - radial * 0.45);
  colour += uNear * caustic;

  float metalSweep = smoothstep(0.035, 0.0, abs(q.x + q.y * 0.42 - sin(t * 1.8) * 0.7));
  colour = mix(colour, uFar, metalSweep * uVelocity * uQuality * 0.055);

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
`;function l(e,t,n,r){let i=e.createShader(t);return i?(e.shaderSource(i,n),e.compileShader(i),e.getShaderParameter(i,e.COMPILE_STATUS)?i:(r(`shader compile failed: ${e.getShaderInfoLog(i)}`),e.deleteShader(i),null)):null}function u(e,t){let n=l(e,e.VERTEX_SHADER,s,t),r=l(e,e.FRAGMENT_SHADER,c,t);if(!n||!r)return n&&e.deleteShader(n),r&&e.deleteShader(r),null;let i=e.createProgram();return i?(e.attachShader(i,n),e.attachShader(i,r),e.linkProgram(i),e.detachShader(i,n),e.detachShader(i,r),e.deleteShader(n),e.deleteShader(r),e.getProgramParameter(i,e.LINK_STATUS)?i:(t(`program link failed: ${e.getProgramInfoLog(i)}`),e.deleteProgram(i),null)):(e.deleteShader(n),e.deleteShader(r),null)}function d(e){let t=String(e||``).trim();if(!t)return null;if(t.charCodeAt(0)===35){let e=t.slice(1),n=e.length===3?e.split(``).map(e=>e+e).join(``):e;return/^[0-9a-f]{6}$/i.test(n)?[0,2,4].map(e=>parseInt(n.slice(e,e+2),16)/255):null}let n=t.match(/-?\d*\.?\d+/g);return!n||n.length<3?null:n.slice(0,3).map(e=>Math.min(1,Math.max(0,Number(e)/255)))}var f=[`uResolution`,`uTime`,`uPointer`,`uFocus`,`uEnergy`,`uActivity`,`uScroll`,`uVelocity`,`uQuality`,`uAlpha`,`uGrain`,`uVoid`,`uNear`,`uFar`,`uGlow`];function p(i,{onDiagnostic:a=()=>{},quality:s=`balanced`,onQualityChange:c=()=>{}}={}){if(!i||typeof i.getContext!=`function`||typeof WebGL2RenderingContext>`u`)return null;let l=null;try{l=i.getContext(`webgl2`,{alpha:!0,antialias:!1,depth:!1,stencil:!1,preserveDrawingBuffer:!1,powerPreference:s===`high`?`high-performance`:`low-power`})}catch(e){a(`webgl2 context refused: ${e}`),l=null}if(!l)return null;let p=u(l,a);if(!p)return null;let m=l.createVertexArray(),h={};for(let e of f)h[e]=l.getUniformLocation(p,e);let g=o[s]?s:`balanced`,_=o[g],v={width:0,height:0,scale:1,time:0,energy:0,targetEnergy:0,pointer:[0,0],targetPointer:[0,0],focus:[0,0],targetFocus:[0,0],scroll:0,targetScroll:0,velocity:0,targetVelocity:0,activity:0,targetActivity:0,alpha:1,grain:.05,colours:{uVoid:[0,0,0],uNear:[0,0,0],uFar:[0,0,0],uGlow:[0,0,0]}},y=null,b=!1,x=!0,S=!1,C=0,w=0,T=0,E=[0,0,1],D=0,O=0,k=[],A=()=>{S||!x||(w=T+n,b||H())},j=()=>Math.abs(v.targetEnergy-v.energy)<r&&Math.abs(v.targetPointer[0]-v.pointer[0])<r&&Math.abs(v.targetPointer[1]-v.pointer[1])<r&&Math.abs(v.targetFocus[0]-v.focus[0])<r&&Math.abs(v.targetFocus[1]-v.focus[1])<r&&Math.abs(v.targetScroll-v.scroll)<r&&Math.abs(v.targetVelocity-v.velocity)<r&&Math.abs(v.targetActivity-v.activity)<r,M=(e,t,n)=>{if(S)return;E=[e,t,n];let r=Math.min(Math.max(n||1,1),_.dprCap)*_.renderScale,a=Math.max(1,Math.round(e*r)),o=Math.max(1,Math.round(t*r));a===v.width&&o===v.height||(v.width=a,v.height=o,i.width=a,i.height=o,l.viewport(0,0,a,o),A())},N=e=>{for(let t of[`uVoid`,`uNear`,`uFar`,`uGlow`]){let n=d(e?.[t]);n&&(v.colours[t]=n)}Number.isFinite(e?.alpha)&&(v.alpha=Math.min(1,Math.max(0,e.alpha))),Number.isFinite(e?.grain)&&(v.grain=Math.min(.4,Math.max(0,e.grain))),A()},P=e=>{if(!Number.isFinite(e))return;let t=Math.min(1,Math.max(0,e));t!==v.targetEnergy&&(v.targetEnergy=t,A())},F=(e,t)=>{let n=[Math.min(1,Math.max(-1,Number(e)||0)),Math.min(1,Math.max(-1,Number(t)||0))];n[0]===v.targetPointer[0]&&n[1]===v.targetPointer[1]||(v.targetPointer=n,A())},I=(e,t)=>{let n=[Math.min(1,Math.max(-1,Number(e)||0)),Math.min(1,Math.max(-1,Number(t)||0))];n[0]===v.targetFocus[0]&&n[1]===v.targetFocus[1]||(v.targetFocus=n,A())},L=(e,t=0)=>{v.targetScroll=Math.min(1,Math.max(-1,Number(e)||0)),v.targetVelocity=Math.min(1,Math.max(0,Number(t)||0)),A()},R=e=>{if(!Number.isFinite(e))return;let t=Math.min(1,Math.max(0,e));t!==v.targetActivity&&(v.targetActivity=t,A())},z=(e,t=`preference`)=>{!o[e]||e===g||(g=e,_=o[e],k=[],O=0,M(E[0],E[1],E[2]),c(e,t),e===`still`?G():A())},B=e=>{if(!Number.isFinite(e)||g===`low`||g===`still`||(k.push(e),k.length<45))return;let t=[...k].sort((e,t)=>e-t),n=t[Math.min(t.length-1,Math.floor(t.length*.95))];k=[],O=n>8?O+1:0,!(O<3)&&z(g===`high`?`balanced`:`low`,`frame-budget`)},V=e=>{if(S||!b)return;if(l.isContextLost()){b=!1,y=null;return}let n=T<w?_.activeFrameMs:_.idleFrameMs;if(D&&e-D<n){y=t(V);return}D=e;let r=C?Math.min(64,e-C):16;C=e,T+=r,v.time+=r/1e3;let i=1-.0016**(r/1e3);v.energy+=(v.targetEnergy-v.energy)*i,v.pointer[0]+=(v.targetPointer[0]-v.pointer[0])*i,v.pointer[1]+=(v.targetPointer[1]-v.pointer[1])*i,v.focus[0]+=(v.targetFocus[0]-v.focus[0])*i,v.focus[1]+=(v.targetFocus[1]-v.focus[1])*i,v.scroll+=(v.targetScroll-v.scroll)*i,v.velocity+=(v.targetVelocity-v.velocity)*i,v.activity+=(v.targetActivity-v.activity)*i,v.targetVelocity*=.12**(r/1e3),v.targetActivity*=.55**(r/1e3);let a=typeof performance<`u`?performance.now():0;if(l.useProgram(p),l.bindVertexArray(m),l.uniform2f(h.uResolution,v.width,v.height),l.uniform1f(h.uTime,v.time),l.uniform2f(h.uPointer,v.pointer[0],v.pointer[1]),l.uniform2f(h.uFocus,v.focus[0],v.focus[1]),l.uniform1f(h.uEnergy,v.energy),l.uniform1f(h.uActivity,v.activity),l.uniform1f(h.uScroll,v.scroll),l.uniform1f(h.uVelocity,v.velocity),l.uniform1f(h.uQuality,_.shaderQuality),l.uniform1f(h.uAlpha,v.alpha),l.uniform1f(h.uGrain,v.grain),l.uniform3fv(h.uVoid,v.colours.uVoid),l.uniform3fv(h.uNear,v.colours.uNear),l.uniform3fv(h.uFar,v.colours.uFar),l.uniform3fv(h.uGlow,v.colours.uGlow),l.drawArrays(l.TRIANGLES,0,3),a&&B(performance.now()-a),T>w&&j()){b=!1,y=null;return}y=t(V)};function H(){S||!x||b||l.isContextLost()||g===`still`||(b=!0,C=0,D=0,w<=T&&(w=T+n),y=t(V))}let U=()=>{b=!1,y!=null&&e(y),y=null},W=e=>{x=!!e,x?A():U()},G=()=>{S||!x||(v.energy=v.targetEnergy,v.pointer=[...v.targetPointer],v.focus=[...v.targetFocus],v.scroll=v.targetScroll,v.velocity=0,v.activity=v.targetActivity,C=0,D=0,b=!0,V(0),U())};return{resize:M,setPalette:N,setEnergy:P,setPointer:F,setFocus:I,setScroll:L,setActivity:R,setQuality:z,setEnabled:W,start:H,stop:U,renderStill:G,dispose:()=>{U(),S=!0;try{l.deleteVertexArray(m),l.deleteProgram(p)}catch{}}}}export{p as a,i,f as n,a as o,s as r,d as s,c as t};