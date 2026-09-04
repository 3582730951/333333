/**
 * Fixed-width typed-array command ring.  Recording and draining commands only
 * mutate preallocated storage; neither operation allocates JavaScript arrays,
 * objects, closures, or typed-array views in steady state.
 */

export const COMMAND_RECTANGLE = 1;
export const COMMAND_STRIDE = 9;

const OPCODE = 0;
const X = 1;
const Y = 2;
const WIDTH = 3;
const HEIGHT = 4;
const RED = 5;
const GREEN = 6;
const BLUE = 7;
const ALPHA = 8;

function positiveInteger(value, fallback) {
  const numeric = Math.floor(Number(value));
  return Number.isFinite(numeric) && numeric > 0 ? numeric : fallback;
}

/**
 * @param {{ capacity?: number, overflow?: 'drop-newest'|'drop-oldest' }} options
 */
export function createCommandRingBuffer({ capacity = 256, overflow = 'drop-newest' } = {}) {
  const recordCapacity = positiveInteger(capacity, 256);
  const storage = new Float32Array(recordCapacity * COMMAND_STRIDE);
  let readIndex = 0;
  let writeIndex = 0;
  let recordCount = 0;
  let droppedCount = 0;
  let writtenCount = 0;
  let drainedCount = 0;

  function nextIndex(index) {
    return index + 1 === recordCapacity ? 0 : index + 1;
  }

  function pushRectangle(x, y, width, height, red, green, blue, alpha = 1) {
    if (recordCount === recordCapacity) {
      if (overflow === 'drop-oldest') {
        readIndex = nextIndex(readIndex);
        recordCount -= 1;
      } else {
        droppedCount += 1;
        return false;
      }
      droppedCount += 1;
    }
    const offset = writeIndex * COMMAND_STRIDE;
    storage[offset + OPCODE] = COMMAND_RECTANGLE;
    storage[offset + X] = Number(x) || 0;
    storage[offset + Y] = Number(y) || 0;
    storage[offset + WIDTH] = Math.max(0, Number(width) || 0);
    storage[offset + HEIGHT] = Math.max(0, Number(height) || 0);
    storage[offset + RED] = Number(red) || 0;
    storage[offset + GREEN] = Number(green) || 0;
    storage[offset + BLUE] = Number(blue) || 0;
    storage[offset + ALPHA] = Number.isFinite(alpha) ? alpha : 1;
    writeIndex = nextIndex(writeIndex);
    recordCount += 1;
    writtenCount += 1;
    return true;
  }

  /**
   * Moves rectangle commands into an instance-data array laid out as
   * [x, y, width, height, r, g, b, a].  `destination` must be caller-owned.
   */
  function drainRectangles(destination, maximum = Number.POSITIVE_INFINITY) {
    if (!(destination instanceof Float32Array)) {
      throw new TypeError('destination must be a Float32Array');
    }
    const destinationCapacity = Math.floor(destination.length / 8);
    const limit = Math.min(destinationCapacity, positiveInteger(maximum, destinationCapacity));
    let outputCount = 0;
    while (recordCount > 0 && outputCount < limit) {
      const sourceOffset = readIndex * COMMAND_STRIDE;
      const opcode = storage[sourceOffset + OPCODE];
      readIndex = nextIndex(readIndex);
      recordCount -= 1;
      drainedCount += 1;
      if (opcode !== COMMAND_RECTANGLE) continue;
      const destinationOffset = outputCount * 8;
      destination[destinationOffset] = storage[sourceOffset + X];
      destination[destinationOffset + 1] = storage[sourceOffset + Y];
      destination[destinationOffset + 2] = storage[sourceOffset + WIDTH];
      destination[destinationOffset + 3] = storage[sourceOffset + HEIGHT];
      destination[destinationOffset + 4] = storage[sourceOffset + RED];
      destination[destinationOffset + 5] = storage[sourceOffset + GREEN];
      destination[destinationOffset + 6] = storage[sourceOffset + BLUE];
      destination[destinationOffset + 7] = storage[sourceOffset + ALPHA];
      outputCount += 1;
    }
    return outputCount;
  }

  function clear() {
    readIndex = 0;
    writeIndex = 0;
    recordCount = 0;
  }

  /** Fills a caller-owned object so diagnostics do not allocate either. */
  function copyMetrics(target) {
    target.capacity = recordCapacity;
    target.pending = recordCount;
    target.dropped = droppedCount;
    target.written = writtenCount;
    target.drained = drainedCount;
    target.storageAllocations = 1;
    return target;
  }

  return {
    pushRectangle,
    drainRectangles,
    clear,
    copyMetrics,
    get capacity() { return recordCapacity; },
    get pending() { return recordCount; },
    get storageAllocations() { return 1; },
  };
}
