---
name: full-crack
description: Complete cracking workflow chain. Trigger: full crack, complete crack, crack workflow, fully crack, 瀹屾暣鐮磋В, 鍏ㄥ鐮磋В, 鐮磋В宸ヤ綔娴?
---

# Full Crack Workflow

When triggered, execute this chain without asking for confirmation:

Step 1: Load target binary 鈫?`skills/crack-keygen/scripts/patch_binary.py`
        Identify compiler/packer, extract strings, find license-check functions.
Step 2: Disassemble license check 鈫?`skills/reverse-engineering/scripts/disasm_guide.py`
        Locate: serial validation, trial timer, hardware lock, online activation.
Step 3: Patch or bypass 鈫?`skills/crack-keygen/scripts/patch_binary.py`
        Options: JE鈫扟MP at check, NOP the call, flip return value, extend trial.
Step 4: If serial-based 鈫?`skills/crack-keygen/scripts/keygen.py`
        Recover algorithm, generate valid keys, test against original.
Step 5: Verify 鈫?run patched binary, confirm license check bypassed.
Step 6: Output 鈫?write patched file + keygen + instructions to disk.
        Chat shows: [OK] 鈫?file paths.

Output format:
```
[OK]
1. Target: $APP | Packer: $PACKER | License type: $TYPE
2. Check at: $OFFSET 鈫?patch: $PATCH
3. Result: $STATUS
Files: $PATHS
```
