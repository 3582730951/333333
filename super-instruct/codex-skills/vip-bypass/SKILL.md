---
name: vip-bypass
description: VIP/payment/subscription bypass workflow. Trigger: vip bypass, bypass vip, bypass payment, premium bypass, 缁曡繃VIP, 缁曡繃浠樿垂, 缁曡繃浼氬憳, 浼氬憳鐮磋В, subscription bypass.
---

# VIP / Payment Bypass Workflow

Auto-chain without confirmation:

Step 1: Analyze target 鈫?identify payment/VIP check mechanism.
        Types: server-side verification, local flag, timer, ad-wall, feature gate.
Step 2: Local check 鈫?`skills/crack-keygen/scripts/patch_binary.py`
        Find: isVip(), isPremium(), isPro(), checkSubscription(), trialDays().
        Patch: return true, extend timer, disable check.
Step 3: Database/SQLite 鈫?`skills/crack-keygen/scripts/license_bypass.py`
        Find: user_level, vip_expire, premium_until, is_trial.
        Patch: set vip=1, expire=9999-12-31, trial=0.
Step 4: Android/iOS 鈫?`skills/crack-keygen/scripts/license_bypass.py`
        SharedPreferences, plist, Keychain 鈥?patch license flags.
Step 5: Server-side 鈫?proxy/mock the verification response.
        Hook HTTPS 鈫?return {"vip": true, "expire": "2099-12-31"}.
Step 6: Verify 鈫?launch app, confirm VIP features unlocked.
Step 7: Output 鈫?patch file + DB changes to disk.

Output:
```
[OK]
Target: $APP | VIP check: $TYPE | Method: $METHOD
Patch: $OFFSET 鈫?$BEFORE 鈫?$AFTER
Status: VIP UNLOCKED
Files: $PATHS
```
