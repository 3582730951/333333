---
name: code-obfuscate
description: Code obfuscation and deobfuscation 鈥?control flow, string encryption, instruction substitution. Trigger: obfuscate code, deobfuscate, code obfuscation, control flow flatten, string encrypt, 浠ｇ爜娣锋穯, 鍘绘贩娣?
---

# Code Obfuscation / Deobfuscation

## Obfuscation Techniques
1. Control flow flattening 鈥?transform if/while/for into switch-based dispatcher
2. String encryption 鈥?XOR/AES strings, decrypt at runtime
3. Instruction substitution 鈥?replace simple ops with complex equivalents
4. Opaque predicates 鈥?always-true/false conditions that look complex
5. Dead code insertion 鈥?unreachable blocks that confuse static analysis
6. Function splitting 鈥?break functions into fragments, reorder randomly
7. Constant obfuscation 鈥?MBA (Mixed Boolean-Arithmetic) expressions

## Deobfuscation Techniques
1. Pattern matching 鈥?recognize standard flattening dispatch patterns
2. Symbolic execution 鈥?angr/MIAsM to recover original control flow
3. Dynamic analysis 鈥?trace execution, record actual path
4. Taint analysis 鈥?track obfuscated values to source
5. LLVM optimization passes 鈥?run -O3 on lifted IR

## Execution
When triggered:
1. Analyze target for obfuscation indicators (entropy, CFG complexity)
2. Identify obfuscation type(s)
3. Apply matching deobfuscation technique
4. Or: apply obfuscation to protect sensitive code
5. Write result to disk
