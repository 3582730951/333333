/** Global uniforms that the engine owns. Effect manifests must not redeclare
 * parameter definitions for these names. `uTime` is simulation time, not wall
 * time: pause, slow motion, seek, and replay therefore stay deterministic. */
export const ENGINE_GLOBAL_UNIFORMS = Object.freeze([
  'uTime',
  'uDeltaTime',
  'uResolution',
  'uPixelRatio',
  'uQuality',
  'uAtmoVoid',
  'uAtmoNear',
  'uAtmoFar',
  'uAtmoGlow',
]);

function compileShader(gl, type, source, label) {
  const shader = gl.createShader(type);
  if (!shader) throw new Error(`${label}: createShader failed`);
  gl.shaderSource(shader, source);
  gl.compileShader(shader);
  if (gl.getShaderParameter(shader, gl.COMPILE_STATUS)) return shader;
  const diagnostic = gl.getShaderInfoLog(shader) || 'unknown shader error';
  gl.deleteShader(shader);
  throw new Error(`${label}: ${diagnostic}`);
}

export function createProgramBinding(gl, { vertexSource, fragmentSource, label = 'effect' }) {
  const vertex = compileShader(gl, gl.VERTEX_SHADER, vertexSource, `${label} vertex`);
  let fragment = null;
  let program = null;
  let linked = false;
  try {
    fragment = compileShader(gl, gl.FRAGMENT_SHADER, fragmentSource, `${label} fragment`);
    program = gl.createProgram();
    if (!program) throw new Error(`${label}: createProgram failed`);
    gl.attachShader(program, vertex);
    gl.attachShader(program, fragment);
    gl.linkProgram(program);
    if (!gl.getProgramParameter(program, gl.LINK_STATUS)) {
      throw new Error(`${label}: ${gl.getProgramInfoLog(program) || 'program link failed'}`);
    }
    linked = true;
    const globals = Object.create(null);
    for (let index = 0; index < ENGINE_GLOBAL_UNIFORMS.length; index += 1) {
      const name = ENGINE_GLOBAL_UNIFORMS[index];
      globals[name] = gl.getUniformLocation(program, name);
    }
    return { program, globals };
  } catch (error) {
    if (program) gl.deleteProgram(program);
    throw error;
  } finally {
    if (program && linked) {
      gl.detachShader(program, vertex);
      gl.detachShader(program, fragment);
    }
    gl.deleteShader(vertex);
    if (fragment) gl.deleteShader(fragment);
  }
}

function upload1f(gl, location, value) {
  if (location !== null) gl.uniform1f(location, value);
}

function upload2fv(gl, location, value) {
  if (location !== null) gl.uniform2fv(location, value);
}

function upload3fv(gl, location, value) {
  if (location !== null) gl.uniform3fv(location, value);
}

export function bindEngineGlobals(gl, binding, frame) {
  const locations = binding.globals;
  upload1f(gl, locations.uTime, frame.time);
  upload1f(gl, locations.uDeltaTime, frame.deltaTime);
  upload2fv(gl, locations.uResolution, frame.resolution);
  upload1f(gl, locations.uPixelRatio, frame.pixelRatio);
  upload1f(gl, locations.uQuality, frame.qualityFactor);
  upload3fv(gl, locations.uAtmoVoid, frame.palette.void);
  upload3fv(gl, locations.uAtmoNear, frame.palette.near);
  upload3fv(gl, locations.uAtmoFar, frame.palette.far);
  upload3fv(gl, locations.uAtmoGlow, frame.palette.glow);
}

export function disposeProgramBinding(gl, binding) {
  if (!binding?.program) return;
  try {
    gl.deleteProgram(binding.program);
  } catch {
    // A context-loss teardown has no GPU objects left to release.
  }
}
