import { createProgramBinding, disposeProgramBinding } from './gl/program.js';

const RECTANGLE_VERTEX_SOURCE = `#version 300 es
layout(location = 0) in vec2 aCorner;
layout(location = 1) in vec4 aRect;
layout(location = 2) in vec4 aColor;

uniform vec2 uResolution;

out vec4 vColor;

void main() {
  vec2 pixel = aRect.xy + aCorner * aRect.zw;
  vec2 clip = pixel / max(uResolution, vec2(1.0)) * 2.0 - 1.0;
  gl_Position = vec4(clip.x, -clip.y, 0.0, 1.0);
  vColor = aColor;
}
`;

const RECTANGLE_FRAGMENT_SOURCE = `#version 300 es
precision mediump float;

in vec4 vColor;
out vec4 outColor;

void main() {
  outColor = vColor;
}
`;

function positiveInteger(value, fallback) {
  const numeric = Math.floor(Number(value));
  return Number.isFinite(numeric) && numeric > 0 ? numeric : fallback;
}

/** Draws a command-buffer batch with one instanced WebGL2 call. */
export function createRectangleRenderer(gl, { capacity = 256 } = {}) {
  const instanceCapacity = positiveInteger(capacity, 256);
  const binding = createProgramBinding(gl, {
    vertexSource: RECTANGLE_VERTEX_SOURCE,
    fragmentSource: RECTANGLE_FRAGMENT_SOURCE,
    label: 'rectangle renderer',
  });
  const vao = gl.createVertexArray();
  const cornerBuffer = gl.createBuffer();
  const instanceBuffer = gl.createBuffer();
  if (!vao || !cornerBuffer || !instanceBuffer) {
    if (vao) gl.deleteVertexArray(vao);
    if (cornerBuffer) gl.deleteBuffer(cornerBuffer);
    if (instanceBuffer) gl.deleteBuffer(instanceBuffer);
    disposeProgramBinding(gl, binding);
    throw new Error('rectangle renderer buffer allocation failed');
  }

  const staging = new Float32Array(instanceCapacity * 8);
  gl.bindVertexArray(vao);
  gl.bindBuffer(gl.ARRAY_BUFFER, cornerBuffer);
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([
    0, 0,
    1, 0,
    0, 1,
    1, 1,
  ]), gl.STATIC_DRAW);
  gl.enableVertexAttribArray(0);
  gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);

  gl.bindBuffer(gl.ARRAY_BUFFER, instanceBuffer);
  gl.bufferData(gl.ARRAY_BUFFER, staging.byteLength, gl.DYNAMIC_DRAW);
  gl.enableVertexAttribArray(1);
  gl.vertexAttribPointer(1, 4, gl.FLOAT, false, 32, 0);
  gl.vertexAttribDivisor(1, 1);
  gl.enableVertexAttribArray(2);
  gl.vertexAttribPointer(2, 4, gl.FLOAT, false, 32, 16);
  gl.vertexAttribDivisor(2, 1);
  gl.bindVertexArray(null);

  let drawCount = 0;

  function render(commandBuffer, frame) {
    const count = commandBuffer.drainRectangles(staging, instanceCapacity);
    if (count === 0) return 0;
    gl.useProgram(binding.program);
    if (binding.globals.uResolution !== null) gl.uniform2fv(binding.globals.uResolution, frame.resolution);
    gl.bindVertexArray(vao);
    gl.bindBuffer(gl.ARRAY_BUFFER, instanceBuffer);
    // WebGL2's srcOffset/length overload avoids creating a subarray view per frame.
    gl.bufferSubData(gl.ARRAY_BUFFER, 0, staging, 0, count * 8);
    gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, count);
    drawCount += 1;
    return count;
  }

  function copyMetrics(target) {
    target.capacity = instanceCapacity;
    target.drawCount = drawCount;
    target.storageAllocations = 2;
    return target;
  }

  function dispose() {
    try {
      gl.deleteVertexArray(vao);
      gl.deleteBuffer(cornerBuffer);
      gl.deleteBuffer(instanceBuffer);
    } catch {
      // Context loss makes deletion unnecessary and sometimes throws.
    }
    disposeProgramBinding(gl, binding);
  }

  return { render, copyMetrics, dispose, get storageAllocations() { return 2; } };
}
