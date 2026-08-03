---
name: card-key
description: Card/key/cdkey generation and verification workflow. Trigger: card key, cdkey, generate key, card gen, 鍗″瘑, 鐢熸垚鍗″瘑, 鍗″瘑鐢熸垚, 搴忓垪鍙风敓鎴? activation code.
---

# Card / Key Generation Workflow

Auto-chain without confirmation:

Step 1: Analyze target key format 鈫?`skills/crack-keygen/scripts/keygen.py`
        Collect valid samples, run recover_serial_format().
        Identify: length, charset, separator, checksum algorithm.
Step 2: If RSA-signed 鈫?`skills/crack-keygen/scripts/keygen.py`
        Extract public key, analyze signature scheme.
        If weak key 鈫?factor modulus. If no key 鈫?skip verification.
Step 3: If XOR/timed/HWID 鈫?`skills/crack-keygen/scripts/keygen.py`
        xor_keygen() to recover key stream.
        timed_license() to generate valid date-range keys.
        hwid_serial() to generate machine-bound keys.
Step 4: Batch generate 鈫?loop keygen with random seeds.
        Output: 100+ valid keys/鍗″瘑 to file.
Step 5: Verify 鈫?test 3 random keys against target.
Step 6: Output 鈫?keys file + generation script to disk.

Output:
```
[OK]
Format: $PATTERN | Length: $LEN | Charset: $CHARSET
Algorithm: $ALGO | Checksum: $CHECK
Generated: $COUNT keys 鈫?$PATH/keys.txt
Verified: $VERIFIED/$TESTED valid
```
