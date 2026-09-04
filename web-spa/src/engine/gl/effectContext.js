import {
  bindEngineGlobals,
  createProgramBinding,
  disposeProgramBinding,
} from './program.js';

/**
 * Gives effects the shared fullscreen VAO and audited shader helpers. Effects
 * may compile their own program, but may not resize, clear, or replace the
 * default framebuffer; the compositor owns those operations.
 */
export function createEffectContext(gl, { onDiagnostic = () => {} } = {}) {
  const fullscreenVao = gl.createVertexArray();
  if (!fullscreenVao) throw new Error('createVertexArray failed');
  gl.bindVertexArray(fullscreenVao);

  function createProgram(sources) {
    try {
      return createProgramBinding(gl, sources);
    } catch (error) {
      onDiagnostic(error instanceof Error ? error.message : String(error));
      throw error;
    }
  }

  function drawFullscreen(binding, frame) {
    gl.useProgram(binding.program);
    gl.bindVertexArray(fullscreenVao);
    bindEngineGlobals(gl, binding, frame);
    gl.drawArrays(gl.TRIANGLES, 0, 3);
  }

  function dispose() {
    try {
      gl.deleteVertexArray(fullscreenVao);
    } catch {
      // Safe during context-loss cleanup.
    }
  }

  return {
    gl,
    fullscreenVao,
    createProgram,
    drawFullscreen,
    bindEngineGlobals(binding, frame) {
      bindEngineGlobals(gl, binding, frame);
    },
    disposeProgram(binding) {
      disposeProgramBinding(gl, binding);
    },
    dispose,
  };
}
