import{u as e}from"./browserStorage-BOQnGVsI.js";import{n as t,t as n}from"./jsx-runtime-B2iFcflP.js";import{a as r}from"./browserDocument-D4IUFITR.js";import{Tt as i,_t as a,bt as o,vt as s,wt as c}from"./RequestIDLine-O5Br2or2.js";import{o as l,t as u}from"./bootstrap-DzFegTRw.js";var d=e(t(),1),f=4e3,p=.002,m=[`auto`,`high`,`balanced`,`low`,`still`,`off`];function h(e){let t=String(e||``).trim().toLowerCase();return m.includes(t)?t:`auto`}var g={high:{dprCap:1.5,renderScale:.78,activeFrameMs:16,idleFrameMs:33,shaderQuality:1},balanced:{dprCap:1.25,renderScale:.7,activeFrameMs:33,idleFrameMs:33,shaderQuality:.68},low:{dprCap:1,renderScale:.62,activeFrameMs:50,idleFrameMs:66,shaderQuality:.28},still:{dprCap:1,renderScale:.62,activeFrameMs:1/0,idleFrameMs:1/0,shaderQuality:.28}},_=`#version 300 es
// Fullscreen triangle, no vertex buffer: gl_VertexID drives the three corners.
// Cheaper than a quad (no diagonal seam, three vertices instead of six) and it
// needs no attribute state to be bound or torn down.
out vec2 vUv;
void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`,v=`#version 300 es
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
`;function y(e,t,n,r){let i=e.createShader(t);return i?(e.shaderSource(i,n),e.compileShader(i),e.getShaderParameter(i,e.COMPILE_STATUS)?i:(r(`shader compile failed: ${e.getShaderInfoLog(i)}`),e.deleteShader(i),null)):null}function b(e,t){let n=y(e,e.VERTEX_SHADER,_,t),r=y(e,e.FRAGMENT_SHADER,v,t);if(!n||!r)return n&&e.deleteShader(n),r&&e.deleteShader(r),null;let i=e.createProgram();return i?(e.attachShader(i,n),e.attachShader(i,r),e.linkProgram(i),e.detachShader(i,n),e.detachShader(i,r),e.deleteShader(n),e.deleteShader(r),e.getProgramParameter(i,e.LINK_STATUS)?i:(t(`program link failed: ${e.getProgramInfoLog(i)}`),e.deleteProgram(i),null)):(e.deleteShader(n),e.deleteShader(r),null)}function x(e){let t=String(e||``).trim();if(!t)return null;if(t.charCodeAt(0)===35){let e=t.slice(1),n=e.length===3?e.split(``).map(e=>e+e).join(``):e;return/^[0-9a-f]{6}$/i.test(n)?[0,2,4].map(e=>parseInt(n.slice(e,e+2),16)/255):null}let n=t.match(/-?\d*\.?\d+/g);return!n||n.length<3?null:n.slice(0,3).map(e=>Math.min(1,Math.max(0,Number(e)/255)))}var S=[`uResolution`,`uTime`,`uPointer`,`uFocus`,`uEnergy`,`uActivity`,`uScroll`,`uVelocity`,`uQuality`,`uAlpha`,`uGrain`,`uVoid`,`uNear`,`uFar`,`uGlow`];function C(e,{onDiagnostic:t=()=>{},quality:n=`balanced`,onQualityChange:r=()=>{}}={}){if(!e||typeof e.getContext!=`function`||typeof WebGL2RenderingContext>`u`)return null;let a=null;try{a=e.getContext(`webgl2`,{alpha:!0,antialias:!1,depth:!1,stencil:!1,preserveDrawingBuffer:!1,powerPreference:n===`high`?`high-performance`:`low-power`})}catch(e){t(`webgl2 context refused: ${e}`),a=null}if(!a)return null;let s=b(a,t);if(!s)return null;let c=a.createVertexArray(),l={};for(let e of S)l[e]=a.getUniformLocation(s,e);let u=g[n]?n:`balanced`,d=g[u],m={width:0,height:0,scale:1,time:0,energy:0,targetEnergy:0,pointer:[0,0],targetPointer:[0,0],focus:[0,0],targetFocus:[0,0],scroll:0,targetScroll:0,velocity:0,targetVelocity:0,activity:0,targetActivity:0,alpha:1,grain:.05,colours:{uVoid:[0,0,0],uNear:[0,0,0],uFar:[0,0,0],uGlow:[0,0,0]}},h=null,_=!1,v=!0,y=!1,C=0,w=0,T=0,E=[0,0,1],D=0,O=0,k=[],A=()=>{y||!v||(w=T+f,_||H())},j=()=>Math.abs(m.targetEnergy-m.energy)<p&&Math.abs(m.targetPointer[0]-m.pointer[0])<p&&Math.abs(m.targetPointer[1]-m.pointer[1])<p&&Math.abs(m.targetFocus[0]-m.focus[0])<p&&Math.abs(m.targetFocus[1]-m.focus[1])<p&&Math.abs(m.targetScroll-m.scroll)<p&&Math.abs(m.targetVelocity-m.velocity)<p&&Math.abs(m.targetActivity-m.activity)<p,M=(t,n,r)=>{if(y)return;E=[t,n,r];let i=Math.min(Math.max(r||1,1),d.dprCap)*d.renderScale,o=Math.max(1,Math.round(t*i)),s=Math.max(1,Math.round(n*i));o===m.width&&s===m.height||(m.width=o,m.height=s,e.width=o,e.height=s,a.viewport(0,0,o,s),A())},N=e=>{for(let t of[`uVoid`,`uNear`,`uFar`,`uGlow`]){let n=x(e?.[t]);n&&(m.colours[t]=n)}Number.isFinite(e?.alpha)&&(m.alpha=Math.min(1,Math.max(0,e.alpha))),Number.isFinite(e?.grain)&&(m.grain=Math.min(.4,Math.max(0,e.grain))),A()},P=e=>{if(!Number.isFinite(e))return;let t=Math.min(1,Math.max(0,e));t!==m.targetEnergy&&(m.targetEnergy=t,A())},F=(e,t)=>{let n=[Math.min(1,Math.max(-1,Number(e)||0)),Math.min(1,Math.max(-1,Number(t)||0))];n[0]===m.targetPointer[0]&&n[1]===m.targetPointer[1]||(m.targetPointer=n,A())},I=(e,t)=>{let n=[Math.min(1,Math.max(-1,Number(e)||0)),Math.min(1,Math.max(-1,Number(t)||0))];n[0]===m.targetFocus[0]&&n[1]===m.targetFocus[1]||(m.targetFocus=n,A())},L=(e,t=0)=>{m.targetScroll=Math.min(1,Math.max(-1,Number(e)||0)),m.targetVelocity=Math.min(1,Math.max(0,Number(t)||0)),A()},R=e=>{if(!Number.isFinite(e))return;let t=Math.min(1,Math.max(0,e));t!==m.targetActivity&&(m.targetActivity=t,A())},z=(e,t=`preference`)=>{!g[e]||e===u||(u=e,d=g[e],k=[],O=0,M(E[0],E[1],E[2]),r(e,t),e===`still`?G():A())},B=e=>{if(!Number.isFinite(e)||u===`low`||u===`still`||(k.push(e),k.length<45))return;let t=[...k].sort((e,t)=>e-t),n=t[Math.min(t.length-1,Math.floor(t.length*.95))];k=[],O=n>8?O+1:0,!(O<3)&&z(u===`high`?`balanced`:`low`,`frame-budget`)},V=e=>{if(y||!_)return;if(a.isContextLost()){_=!1,h=null;return}let t=T<w?d.activeFrameMs:d.idleFrameMs;if(D&&e-D<t){h=i(V);return}D=e;let n=C?Math.min(64,e-C):16;C=e,T+=n,m.time+=n/1e3;let r=1-.0016**(n/1e3);m.energy+=(m.targetEnergy-m.energy)*r,m.pointer[0]+=(m.targetPointer[0]-m.pointer[0])*r,m.pointer[1]+=(m.targetPointer[1]-m.pointer[1])*r,m.focus[0]+=(m.targetFocus[0]-m.focus[0])*r,m.focus[1]+=(m.targetFocus[1]-m.focus[1])*r,m.scroll+=(m.targetScroll-m.scroll)*r,m.velocity+=(m.targetVelocity-m.velocity)*r,m.activity+=(m.targetActivity-m.activity)*r,m.targetVelocity*=.12**(n/1e3),m.targetActivity*=.55**(n/1e3);let o=typeof performance<`u`?performance.now():0;if(a.useProgram(s),a.bindVertexArray(c),a.uniform2f(l.uResolution,m.width,m.height),a.uniform1f(l.uTime,m.time),a.uniform2f(l.uPointer,m.pointer[0],m.pointer[1]),a.uniform2f(l.uFocus,m.focus[0],m.focus[1]),a.uniform1f(l.uEnergy,m.energy),a.uniform1f(l.uActivity,m.activity),a.uniform1f(l.uScroll,m.scroll),a.uniform1f(l.uVelocity,m.velocity),a.uniform1f(l.uQuality,d.shaderQuality),a.uniform1f(l.uAlpha,m.alpha),a.uniform1f(l.uGrain,m.grain),a.uniform3fv(l.uVoid,m.colours.uVoid),a.uniform3fv(l.uNear,m.colours.uNear),a.uniform3fv(l.uFar,m.colours.uFar),a.uniform3fv(l.uGlow,m.colours.uGlow),a.drawArrays(a.TRIANGLES,0,3),o&&B(performance.now()-o),T>w&&j()){_=!1,h=null;return}h=i(V)};function H(){y||!v||_||a.isContextLost()||u===`still`||(_=!0,C=0,D=0,w<=T&&(w=T+f),h=i(V))}let U=()=>{_=!1,h!=null&&o(h),h=null},W=e=>{v=!!e,v?A():U()},G=()=>{y||!v||(m.energy=m.targetEnergy,m.pointer=[...m.targetPointer],m.focus=[...m.targetFocus],m.scroll=m.targetScroll,m.velocity=0,m.activity=m.targetActivity,C=0,D=0,_=!0,V(0),U())};return{resize:M,setPalette:N,setEnergy:P,setPointer:F,setFocus:I,setScroll:L,setActivity:R,setQuality:z,setEnabled:W,start:H,stop:U,renderStill:G,dispose:()=>{U(),y=!0;try{a.deleteVertexArray(c),a.deleteProgram(s)}catch{}}}}var w=n(),T=`pool.visual_quality`,E=[`--pool-atmo-void`,`--pool-atmo-near`,`--pool-atmo-far`,`--pool-atmo-glow`,`--pool-atmo-alpha`,`--pool-grain-alpha`];function D(){let e=r([...E]);return{uVoid:e[`--pool-atmo-void`],uNear:e[`--pool-atmo-near`],uFar:e[`--pool-atmo-far`],uGlow:e[`--pool-atmo-glow`],alpha:Number(e[`--pool-atmo-alpha`])||1,grain:Number(e[`--pool-grain-alpha`])||0}}function O(){try{return!!window.matchMedia?.(`(prefers-reduced-motion: reduce)`).matches}catch{return!1}}function k(){try{return!!navigator.connection?.saveData}catch{return!1}}function A(){try{return h(localStorage.getItem(T))}catch{return`auto`}}function j(e){if(e===`off`||k())return`off`;if(e===`still`||O())return`still`;if(e!==`auto`)return e;let t=navigator,n=Number(t.hardwareConcurrency)||4,r=Number(t.deviceMemory)||4;return n<=4||r<=4?`low`:n>=8&&r>=8&&window.matchMedia?.(`(min-width: 1024px) and (pointer: fine)`).matches?`high`:`balanced`}function M({subscribe:e}){let t=(0,d.useRef)(null),[n,r]=(0,d.useState)(!1),[i,o]=(0,d.useState)(()=>u()?.ui_experience_v2!==!1),[f,p]=(0,d.useState)(A),[m,g]=(0,d.useState)(()=>j(A())),[_,v]=(0,d.useState)(0),y=(0,d.useMemo)(()=>j(f),[f]);return(0,d.useEffect)(()=>{let e=e=>{let t=h((e instanceof CustomEvent?e.detail:void 0)??A());try{localStorage.setItem(T,t)}catch{}p(t)},t=s(`pool-visual-quality-change`,e),n=s(`storage`,t=>{t.key===T&&e()});return()=>{t(),n()}},[]),(0,d.useEffect)(()=>{let n=t.current;if(!n)return;if(g(y),y===`off`){r(!1);return}let u=C(n,{quality:y,onQualityChange:e=>{(e===`high`||e===`balanced`||e===`low`||e===`still`)&&g(e)},onDiagnostic:e=>l(Error(e),{source:`atmosphere.webgl`,componentStack:`AtmosphereLayer`})});if(!u)return;let d=y===`still`,f=!1;u.setEnabled(i),r(i);let p=()=>{if(f)return;let e=n.getBoundingClientRect();u.resize(e.width,e.height,window.devicePixelRatio||1),d&&u.renderStill()};u.setPalette(D()),p();let m=new MutationObserver(()=>{f||(u.setPalette(D()),d&&u.renderStill())});m.observe(document.documentElement,{attributes:!0,attributeFilter:[`data-theme`]});let h=typeof ResizeObserver==`function`?new ResizeObserver(p):null;h?.observe(n);let _=h?()=>{}:s(`resize`,p),b=()=>{},x=()=>{},S=()=>{},w=()=>{},T=()=>{},E=()=>{},O=()=>{};if(d)u.renderStill();else{let e={x:window.innerWidth/2,y:window.innerHeight/2,at:performance.now()};b=s(`pointermove`,t=>{let n=window.innerWidth||1,r=window.innerHeight||1;u.setPointer(t.clientX/n*2-1,t.clientY/r*2-1);let i=performance.now(),a=Math.max(16,i-e.at),o=Math.hypot(t.clientX-e.x,t.clientY-e.y);u.setActivity(Math.min(1,o/a/1.4)),e={x:t.clientX,y:t.clientY,at:i}},{passive:!0});let t={y:window.scrollY,at:performance.now()};x=s(`scroll`,()=>{let e=Math.max(1,document.documentElement.scrollHeight-window.innerHeight),n=performance.now(),r=Math.max(16,n-t.at),i=Math.min(1,Math.abs(window.scrollY-t.y)/r/1.2);u.setScroll(window.scrollY/e*2-1,i),t={y:window.scrollY,at:n}},{passive:!0}),S=a(`focusin`,e=>{if(!(e.target instanceof HTMLElement))return;let t=e.target.getBoundingClientRect(),n=(t.left+t.width/2)/Math.max(1,window.innerWidth)*2-1,r=(t.top+t.height/2)/Math.max(1,window.innerHeight)*2-1;u.setFocus(n,r),u.setActivity(.42)}),T=a(`pointerdown`,()=>u.setActivity(.72),{passive:!0}),w=s(`pool-rpm-activity`,e=>{u.setActivity(Number(e.detail)||0)}),E=a(`visibilitychange`,()=>{f||(c()?u.start():u.stop())}),c()&&u.start()}e&&(O=e(e=>{f||(u.setEnergy(Number(e?.energy)||0),u.setActivity(Number(e?.rpm_activity)||0),typeof e?.ui_experience_v2==`boolean`&&(o(e.ui_experience_v2),u.setEnabled(e.ui_experience_v2),e.ui_experience_v2?r(!0):r(!1)),d&&e?.ui_experience_v2!==!1&&u.renderStill())}));let k=e=>{e.preventDefault(),u.stop(),r(!1)},A=()=>v(e=>e+1);return n.addEventListener(`webglcontextlost`,k),n.addEventListener(`webglcontextrestored`,A),()=>{f=!0,r(!1),O(),b(),x(),S(),w(),T(),E(),_(),h?.disconnect(),m.disconnect(),n.removeEventListener(`webglcontextlost`,k),n.removeEventListener(`webglcontextrestored`,A),u.dispose()}},[_,y,e]),(0,w.jsxs)(`div`,{className:`pool-atmosphere`,"data-webgl":n&&i?`true`:`false`,"data-quality":m,"data-experience":i?`enhanced`:`base`,"aria-hidden":`true`,children:[(0,w.jsx)(`canvas`,{ref:t,className:`pool-atmosphere__canvas`}),(0,w.jsx)(`div`,{className:`pool-atmosphere__grain`})]})}export{M as default,j as resolveVisualQuality};